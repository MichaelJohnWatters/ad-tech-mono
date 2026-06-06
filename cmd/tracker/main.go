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
	sc := config.Setup(constants.ServiceTracker, trackerSchema, log)
	cfg := sc.Cfg
	knobs := NewKnobs(sc)
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
	dedup := NewDedup(l2, knobs.DedupTTL.Value, knobs.DedupEnabled.Value, log)

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

		// Validate HMAC signature. When tracker.signature_validation is
		// true (prod-shape) we 403 the request; otherwise we warn and let
		// the event through so dev pipelines that don't yet sign keep
		// flowing. The config knob is live-tunable so ops can ratchet
		// strictness without a redeploy.
		if !adserving.ValidateSignature(r.URL.Path, q, signingKey) {
			reqLog.Warn("invalid signature", "path", r.URL.Path)
			if cfg.GetBool("tracker.signature_validation", false) {
				go publisher.publishRejected(context.WithoutCancel(ctx),
					"impression", "invalid_signature", "", traceID, reqLog)
				http.Error(w, "invalid signature", http.StatusForbidden)
				return
			}
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
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"impression", "fraud", strings.Join(fraudResult.Reasons, ","), traceID, reqLog)
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
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"impression", "dedup", "", traceID, reqLog)
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
			w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
			w.Write(pixel)
			return
		}

		// BidModel carried on the URL via ?bm= so the billing engine can
		// route reserve-vs-bill-immediately correctly. Default CPM when
		// missing (legacy URLs from before this param landed).
		bidModel := q.Get("bm")
		if bidModel == "" {
			bidModel = constants.BidModelCPM
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
			BidModel:         bidModel,
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
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"click", "dedup", "", traceID, reqLog)
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
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"conversion", "dedup", convType, traceID, reqLog)
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

	// Viewability beacon. Client (adtech.js / publisher simulator) calls
	// this after observing the rendered ad in the viewport. URL params:
	//   tid  - trace id (links back to the impression)
	//   cid, crid, pid, pubid, advid - campaign / creative / placement /
	//                                  publisher / advertiser
	//   dur  - milliseconds the ad was visible at >= the IAB threshold
	//   pct  - peak percent of the ad's pixels in viewport (0-100)
	//   area - optional, ad's pixel area (w*h). When provided, the >=242,500
	//          IAB Large Format rule kicks in (30% threshold instead of 50%).
	//
	// Server is the authority on IsIABViewable — the client's bool isn't
	// trusted. The analytics row stores both the measured inputs and the
	// server's verdict so downstream consumers (billing, dashboards) can
	// filter on iab_viewable directly.
	mux.HandleFunc(routes.TrackerView, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)

		// Same HMAC + fraud + dedup gates as the impression handler.
		if !adserving.ValidateSignature(r.URL.Path, q, signingKey) {
			reqLog.Warn("invalid signature", "path", r.URL.Path)
			if cfg.GetBool("tracker.signature_validation", false) {
				go publisher.publishRejected(context.WithoutCancel(ctx),
					"view", "invalid_signature", "", traceID, reqLog)
				http.Error(w, "invalid signature", http.StatusForbidden)
				return
			}
		}

		fraudResult := fraudChecker.Check(fraud.Request{
			IP: r.RemoteAddr, UserAgent: r.UserAgent(),
			TraceID: traceID, Referer: r.Referer(),
		})
		if fraudResult.Blocked {
			reqLog.Warn("view fraud blocked", "score", fraudResult.Score, "reasons", fraudResult.Reasons)
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"view", "fraud", strings.Join(fraudResult.Reasons, ","), traceID, reqLog)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		durMs, _ := strconv.ParseInt(q.Get("dur"), 10, 64)
		pct, _ := strconv.Atoi(q.Get("pct"))
		areaPx, _ := strconv.ParseInt(q.Get("area"), 10, 64)
		iabViewable := analytics.IsIABViewable(durMs, pct, areaPx)

		reqLog.Info("viewability",
			"duration_ms", durMs,
			"percent_visible", pct,
			"area_px", areaPx,
			"iab_viewable", iabViewable,
			"campaign_id", q.Get("cid"),
		)

		// One view per (trace, view) — multiple beacons on the same render
		// (browser back-button replay, double-firing IntersectionObserver) get
		// deduped here, same as impressions.
		if !dedup.FirstSeen(ctx, "view", traceID) {
			reqLog.Debug("duplicate view, dropping", "trace_id", traceID)
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"view", "dedup", "", traceID, reqLog)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		go publisher.publishView(context.WithoutCancel(ctx), analytics.ViewEvent{
			TraceID:        traceID,
			CampaignID:     q.Get("cid"),
			CreativeID:     q.Get("crid"),
			PlacementID:    q.Get("pid"),
			PublisherID:    q.Get("pubid"),
			AccountID:      q.Get("advid"),
			DurationMs:     durMs,
			PercentVisible: pct,
			AreaPx:         areaPx,
			IABViewable:    iabViewable,
			SchemaVersion:  1,
			Timestamp:      time.Now().UTC(),
		}, reqLog)

		// Echo the server's verdict back to the client (the simulator reads
		// this so it can show "IAB viewable (server-verified)" vs the JS-only
		// guess). Header-only — body stays 204.
		if iabViewable {
			w.Header().Set("X-IAB-Viewable", "1")
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Video/Audio events. Publish through the same eventPublisher used by
	// impression/click — bus may be nil in dev-no-NATS mode, in which case
	// the publish is a no-op and we still return 204 so the player keeps
	// firing pings.
	mux.HandleFunc(routes.TrackerVideo, func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		eventType := r.URL.Query().Get("event")
		ctx := logger.WithTraceID(r.Context(), traceID)
		logger.WithContext(log, ctx).Info("video_event", "event_type", eventType)
		go publisher.publishVideo(context.WithoutCancel(ctx), events.VideoEvent{
			TraceID:   traceID,
			EventType: eventType,
			Timestamp: time.Now(),
		}, log)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(routes.TrackerAudio, func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		eventType := r.URL.Query().Get("event")
		ctx := logger.WithTraceID(r.Context(), traceID)
		logger.WithContext(log, ctx).Info("audio_event", "event_type", eventType)
		go publisher.publishAudio(context.WithoutCancel(ctx), events.AudioEvent{
			TraceID:   traceID,
			EventType: eventType,
			Timestamp: time.Now(),
		}, log)
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

func (p *eventPublisher) publishView(ctx context.Context, e analytics.ViewEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectView, e, log) {
		return
	}
}

func (p *eventPublisher) publishConversion(ctx context.Context, e analytics.ConversionEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectConversion, e, log) {
		return
	}
	p.httpFallback(analytics.Event{Type: analytics.EventConversion, Conversion: &e}, log)
}

// publishVideo / publishAudio fire from the /v1/t/video and /v1/t/audio
// pixel handlers respectively. No HTTP fallback — engagement pings are
// observability, not billing source-of-truth, so a NATS outage just
// means the event is lost rather than triggering the standalone path.
func (p *eventPublisher) publishVideo(ctx context.Context, e events.VideoEvent, log *slog.Logger) {
	p.publish(ctx, events.SubjectVideo, e, log)
}
func (p *eventPublisher) publishAudio(ctx context.Context, e events.AudioEvent, log *slog.Logger) {
	p.publish(ctx, events.SubjectAudio, e, log)
}

// publishRejected fires whenever a pixel is dropped at the gate — HMAC
// strict-mode reject, fraud check blocked, or dedup hit. Fire-and-forget
// (no HTTP fallback): the rejection itself isn't billable, so reporting
// losing it isn't a billing-correctness issue, just an analytics gap.
// Called via `go p.publishRejected(...)` from inside the pixel handlers
// so the response path stays sub-10ms.
func (p *eventPublisher) publishRejected(ctx context.Context, eventType, reason, detail, traceID string, log *slog.Logger) {
	p.publish(ctx, events.SubjectTrackerRejected, events.TrackerRejectedEvent{
		TraceID:   traceID,
		EventType: eventType,
		Reason:    reason,
		Detail:    detail,
		Timestamp: time.Now().UTC(),
	}, log)
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
