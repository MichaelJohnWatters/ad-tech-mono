// cmd/tracker records ad events (impressions, clicks, conversions, viewability).
// Internet-facing - hit by end-user browsers via pixel URLs.
// Routed directly by Traefik (bypasses Gateway for performance).
package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
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
	cfg := config.Load()
	log := logger.New("tracker")
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("tracker.port", "8083")
	reportingURL := cfg.Get("tracker.reporting_url", "http://localhost:8086")

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// Impression pixel - all context comes from URL params (set by ad server macros)
	mux.HandleFunc("/v1/t/imp", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)

		price, _ := strconv.ParseFloat(q.Get("price"), 64)

		reqLog.Info("impression",
			"campaign_id", q.Get("cid"),
			"creative_id", q.Get("crid"),
			"placement_id", q.Get("pid"),
			"publisher_id", q.Get("pubid"),
			"price", price,
		)

		// Forward to reporting service
		go forwardEvent(reportingURL, analytics.Event{
			Type: analytics.EventImpression,
			Impression: &analytics.ImpressionEvent{
				TraceID:          traceID,
				CampaignID:       q.Get("cid"),
				CreativeID:       q.Get("crid"),
				PlacementID:      q.Get("pid"),
				PublisherID:      q.Get("pubid"),
				AccountID:        q.Get("advid"),
				Geo:              q.Get("geo"),
				Device:           q.Get("dev"),
				Channel:          "display",
				ClearingPrice:    price,
				ClearingCurrency: q.Get("cur"),
				ClearingPriceUSD: price, // TODO: convert if not USD
				BidModel:         "cpm",
				DealID:           q.Get("deal"),
				SchemaVersion:    1,
				Timestamp:        time.Now().UTC(),
			},
		}, reqLog)

		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Cache-Control", "no-store, no-cache")
		w.Write(pixel)
	})

	// Click redirect
	mux.HandleFunc("/v1/t/click", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		redir := q.Get("redir")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("click",
			"campaign_id", q.Get("cid"),
			"creative_id", q.Get("crid"),
			"redirect", redir,
		)

		go forwardEvent(reportingURL, analytics.Event{
			Type: analytics.EventClick,
			Click: &analytics.ClickEvent{
				TraceID:     traceID,
				CampaignID:  q.Get("cid"),
				CreativeID:  q.Get("crid"),
				PlacementID: q.Get("pid"),
				PublisherID: q.Get("pubid"),
				AccountID:   q.Get("advid"),
				LandingURL:  redir,
				Timestamp:   time.Now().UTC(),
			},
		}, reqLog)

		if redir == "" {
			http.Error(w, "missing redirect URL", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, redir, http.StatusFound)
	})

	// Conversion pixel
	mux.HandleFunc("/v1/t/conv", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		convType := q.Get("type")
		revenue, _ := strconv.ParseFloat(q.Get("rev"), 64)

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("conversion", "type", convType, "revenue", revenue)

		go forwardEvent(reportingURL, analytics.Event{
			Type: analytics.EventConversion,
			Conversion: &analytics.ConversionEvent{
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
			},
		}, reqLog)

		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Cache-Control", "no-store, no-cache")
		w.Write(pixel)
	})

	// Viewability beacon
	mux.HandleFunc("/v1/t/view", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("viewability",
			"duration_ms", q.Get("dur"),
			"percent_visible", q.Get("pct"),
			"campaign_id", q.Get("cid"),
		)

		w.WriteHeader(http.StatusNoContent)
	})

	// Video event
	mux.HandleFunc("/v1/t/video", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		eventType := r.URL.Query().Get("event")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("video_event", "event_type", eventType)
		w.WriteHeader(http.StatusNoContent)
	})

	// Audio event
	mux.HandleFunc("/v1/t/audio", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		eventType := r.URL.Query().Get("event")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("audio_event", "event_type", eventType)
		w.WriteHeader(http.StatusNoContent)
	})

	handler := middleware.CORS(mux)
	server := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	log.Info("tracker starting", "port", port, "reporting_url", reportingURL)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// forwardEvent sends an event to the reporting service over HTTP.
// Runs in a goroutine - fire and forget, don't block the pixel response.
// This bridges the gap until NATS is wired end-to-end.
func forwardEvent(reportingURL string, event analytics.Event, log *slog.Logger) {
	body, err := json.Marshal([]analytics.Event{event})
	if err != nil {
		log.Warn("failed to marshal event for reporting", "error", err)
		return
	}

	resp, err := http.Post(reportingURL+"/v1/reporting/events", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Warn("failed to forward event to reporting", "error", err)
		return
	}
	resp.Body.Close()
}
