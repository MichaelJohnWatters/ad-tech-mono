// cmd/tracker records ad events (impressions, clicks, conversions, viewability).
// Internet-facing - hit by end-user browsers via pixel URLs.
// Publishes events to NATS JetStream. Falls back to HTTP bridge if NATS unavailable.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// 1x1 transparent GIF pixel (43 bytes)
var pixel = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
	0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21,
	0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00,
	0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44,
	0x01, 0x00, 0x3b,
}

func main() {
	log := logger.New(constants.ServiceTracker)
	sc := config.Setup(constants.ServiceTracker, log)
	cfg := sc.Cfg
	_ = sc // manager available for OnChange callbacks
	config.PublishSchemaWithURL(cfg.Get("database.url", ""), constants.ServiceTracker, trackerSchema, log)
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("tracker.port", routes.PortTracker)
	natsURL := cfg.Get("tracker.nats_url", routes.DefaultNATSURL)
	reportingURL := cfg.Get("tracker.reporting_url", routes.DefaultReportingURL)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceTracker,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 0.1), // pixels are high volume — sample only 10% by default
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	// Try NATS first, fall back to HTTP bridge
	var bus events.EventBus
	natsBus, err := natsbus.New(natsURL, constants.ServiceTracker, log)
	if err != nil {
		log.Warn("nats unavailable, using HTTP bridge to reporting", "error", err)
		bus = nil // will use HTTP fallback
	} else {
		// Ensure the adtech stream exists
		ctx := context.Background()
		if err := natsBus.EnsureStream(ctx, "adtech", []string{"adtech.>"}); err != nil {
			log.Warn("failed to create stream, using HTTP bridge", "error", err)
			natsBus.Close()
			bus = nil
		} else {
			bus = natsBus
			lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
			log.Info("nats connected, publishing events to JetStream")
		}
	}

	publisher := &eventPublisher{bus: bus, reportingURL: reportingURL, log: log}
	fraudChecker := fraud.NewRealTimeChecker(fraud.DefaultConfig())
	signingKey := cfg.Get("tracker.signing_key", adserving.DefaultSigningKey)
	metrics := middleware.NewMetrics(constants.ServiceTracker)

	l2 := connectRedis(cfg, log)
	dedup := NewDedup(
		l2,
		cfg.GetDuration("tracker.dedup_ttl", 24*time.Hour),
		cfg.GetBool("tracker.dedup_enabled", true),
		log,
	)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Impression pixel
	mux.HandleFunc(routes.TrackerImpression, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)

		// Validate HMAC signature
		if !adserving.ValidateSignature(r.URL.Path, q, signingKey) {
			reqLog.Warn("invalid signature", "path", r.URL.Path)
			// Don't block in dev - just warn. In prod: return 403.
		}

		// Real-time fraud check
		fraudResult := fraudChecker.Check(fraud.Request{
			IP: r.RemoteAddr, UserAgent: r.UserAgent(),
			TraceID: traceID, Referer: r.Referer(),
		})
		// Dev-mode override: publisher simulator can append ?dev_force_fraud=1
		// to deliberately trip a block, so the UI can demonstrate the
		// fraud-rejection flow. Gated by debug.endpoints_enabled — prod
		// requests can't be forced into the blocked path by a forged param.
		if q.Get("dev_force_fraud") == "1" && cfg.GetBool("debug.endpoints_enabled", true) {
			fraudResult.Blocked = true
			fraudResult.Reasons = append([]string{"dev_force_fraud"}, fraudResult.Reasons...)
		}
		if fraudResult.Blocked {
			reqLog.Warn("fraud blocked", "score", fraudResult.Score, "reasons", fraudResult.Reasons)
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
			// Dev-mode signal so the pub sim UI can show fraud was tripped.
			// Real bots get the silent pixel-return treatment in prod (this
			// header simply isn't set when debug endpoints are off).
			if cfg.GetBool("debug.endpoints_enabled", true) {
				w.Header().Set("X-Dev-Fraud-Blocked", "1")
				w.Header().Set("X-Dev-Fraud-Reasons", strings.Join(fraudResult.Reasons, ","))
			}
			w.Write(pixel) // still return pixel (don't reveal detection)
			return         // but don't record or bill
		}

		price, _ := strconv.ParseFloat(q.Get("price"), 64)
		reqLog.Info("impression",
			"campaign_id", q.Get("cid"),
			"creative_id", q.Get("crid"),
			"placement_id", q.Get("pid"),
			"publisher_id", q.Get("pubid"),
			"price", price,
		)

		if !dedup.FirstSeen(ctx, "impression", traceID) {
			reqLog.Debug("duplicate impression, dropping", "trace_id", traceID)
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
			w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
			w.Write(pixel)
			return
		}

		go publisher.publishImpression(context.WithoutCancel(ctx), analytics.ImpressionEvent{
			TraceID:          traceID,
			CampaignID:       q.Get("cid"),
			CreativeID:       q.Get("crid"),
			PlacementID:      q.Get("pid"),
			PublisherID:      q.Get("pubid"),
			AccountID:        q.Get("advid"),
			Geo:              q.Get("geo"),
			Device:           q.Get("dev"),
			Channel:          constants.ChannelDisplay,
			ClearingPrice:    price,
			ClearingCurrency: q.Get("cur"),
			ClearingPriceUSD: price,
			BidModel:         constants.BidModelCPM,
			DealID:           q.Get("deal"),
			SchemaVersion:    1,
			Timestamp:        time.Now().UTC(),
		}, reqLog)

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
		w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
		w.Write(pixel)
	})

	// Click redirect
	mux.HandleFunc(routes.TrackerClick, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		redir := q.Get("redir")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("click", "campaign_id", q.Get("cid"), "redirect", redir)

		if !dedup.FirstSeen(ctx, "click", traceID) {
			reqLog.Debug("duplicate click, dropping", "trace_id", traceID)
			if redir != "" {
				http.Redirect(w, r, redir, http.StatusFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		go publisher.publishClick(context.WithoutCancel(ctx), analytics.ClickEvent{
			TraceID:     traceID,
			CampaignID:  q.Get("cid"),
			CreativeID:  q.Get("crid"),
			PlacementID: q.Get("pid"),
			PublisherID: q.Get("pubid"),
			AccountID:   q.Get("advid"),
			LandingURL:  redir,
			Timestamp:   time.Now().UTC(),
		}, reqLog)

		if redir == "" {
			http.Error(w, "missing redirect URL", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, redir, http.StatusFound)
	})

	// Conversion pixel
	mux.HandleFunc(routes.TrackerConversion, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		convType := q.Get("type")
		revenue, _ := strconv.ParseFloat(q.Get("rev"), 64)
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("conversion", "type", convType, "revenue", revenue)

		if !dedup.FirstSeen(ctx, "conversion:"+convType, traceID) {
			reqLog.Debug("duplicate conversion, dropping", "trace_id", traceID, "type", convType)
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
			w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
			w.Write(pixel)
			return
		}

		go publisher.publishConversion(context.WithoutCancel(ctx), analytics.ConversionEvent{
			TraceID:        traceID,
			CampaignID:     q.Get("cid"),
			CreativeID:     q.Get("crid"),
			PlacementID:    q.Get("pid"),
			AccountID:      q.Get("advid"),
			ConversionType: convType,
			Revenue:        revenue,
			Currency:       q.Get("cur"),
			RevenueUSD:     revenue,
			Timestamp:      time.Now().UTC(),
		}, reqLog)

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
		w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
		w.Write(pixel)
	})

	// Viewability beacon
	mux.HandleFunc(routes.TrackerView, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("viewability", "duration_ms", q.Get("dur"), "percent_visible", q.Get("pct"), "campaign_id", q.Get("cid"))
		w.WriteHeader(http.StatusNoContent)
	})

	// Video/Audio events
	mux.HandleFunc(routes.TrackerVideo, func(w http.ResponseWriter, r *http.Request) {
		ctx := logger.WithTraceID(r.Context(), r.URL.Query().Get("tid"))
		logger.WithContext(log, ctx).Info("video_event", "event_type", r.URL.Query().Get("event"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(routes.TrackerAudio, func(w http.ResponseWriter, r *http.Request) {
		ctx := logger.WithTraceID(r.Context(), r.URL.Query().Get("tid"))
		logger.WithContext(log, ctx).Info("audio_event", "event_type", r.URL.Query().Get("event"))
		w.WriteHeader(http.StatusNoContent)
	})

	handler := tracing.HTTPMiddleware(constants.ServiceTracker)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}

	mode := "NATS JetStream"
	if bus == nil {
		mode = "HTTP bridge"
	}
	log.Info("tracker starting", "port", port, "mode", mode)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// eventPublisher handles publishing to NATS or falling back to HTTP.
type eventPublisher struct {
	bus          events.EventBus
	reportingURL string
	log          *slog.Logger
}

func (p *eventPublisher) publishImpression(ctx context.Context, e analytics.ImpressionEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectImpression, e, log) {
		return
	}
	p.httpFallback(analytics.Event{Type: analytics.EventImpression, Impression: &e}, log)
}

func (p *eventPublisher) publishClick(ctx context.Context, e analytics.ClickEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectClick, e, log) {
		return
	}
	p.httpFallback(analytics.Event{Type: analytics.EventClick, Click: &e}, log)
}

func (p *eventPublisher) publishConversion(ctx context.Context, e analytics.ConversionEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectConversion, e, log) {
		return
	}
	p.httpFallback(analytics.Event{Type: analytics.EventConversion, Conversion: &e}, log)
}

// publish marshals and publishes to NATS. Returns true if successful.
func (p *eventPublisher) publish(ctx context.Context, subject string, payload interface{}, log *slog.Logger) bool {
	if p.bus == nil {
		return false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		log.Warn("marshal failed", "subject", subject, "error", err)
		return false
	}
	if err := p.bus.Publish(ctx, subject, data); err != nil {
		log.Warn("nats publish failed, falling back to HTTP", "subject", subject, "error", err)
		return false
	}
	return true
}

func (p *eventPublisher) httpFallback(event analytics.Event, log *slog.Logger) {
	body, err := json.Marshal([]analytics.Event{event})
	if err != nil {
		log.Warn("marshal for http fallback failed", "error", err)
		return
	}
	resp, err := http.Post(p.reportingURL+routes.ReportingEvents, constants.ContentTypeJSON, bytes.NewReader(body))
	if err != nil {
		log.Warn("http fallback failed", "error", err)
		return
	}
	resp.Body.Close()
}
