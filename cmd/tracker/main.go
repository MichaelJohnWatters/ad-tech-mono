// cmd/tracker records ad events (impressions, clicks, conversions, viewability).
// Internet-facing - hit by end-user browsers via pixel URLs.
// Routed directly by Traefik (bypasses Gateway for performance).
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
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
	slog := logger.New("tracker")
	hlth := health.New()
	lc := lifecycle.New(slog)

	port := cfg.Get("tracker.port", "8083")

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// Impression pixel
	mux.HandleFunc("/v1/t/imp", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		campaignID := r.URL.Query().Get("cid")
		placementID := r.URL.Query().Get("pid")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(slog, ctx)

		// TODO(phase2-wire): Validate sig, fraud checks, publish to NATS
		reqLog.Info("impression",
			"campaign_id", campaignID,
			"placement_id", placementID,
		)

		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Cache-Control", "no-store, no-cache")
		w.Write(pixel)
	})

	// Click redirect
	mux.HandleFunc("/v1/t/click", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		redir := r.URL.Query().Get("redir")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(slog, ctx)

		// TODO(phase2-wire): Validate sig, publish ClickEvent to NATS
		reqLog.Info("click", "redirect", redir)

		if redir == "" {
			http.Error(w, "missing redirect URL", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, redir, http.StatusFound)
	})

	// Conversion pixel
	mux.HandleFunc("/v1/t/conv", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		convType := r.URL.Query().Get("type")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(slog, ctx)

		// TODO(phase2-wire): Validate sig, publish ConversionEvent to NATS
		reqLog.Info("conversion", "type", convType)

		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Cache-Control", "no-store, no-cache")
		w.Write(pixel)
	})

	// Viewability beacon
	mux.HandleFunc("/v1/t/view", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		dur := r.URL.Query().Get("dur")
		pct := r.URL.Query().Get("pct")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(slog, ctx)

		// TODO(phase2-wire): Publish ViewabilityEvent to NATS
		reqLog.Info("viewability", "duration_ms", dur, "percent_visible", pct)

		w.WriteHeader(http.StatusNoContent)
	})

	// Video event
	mux.HandleFunc("/v1/t/video", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		eventType := r.URL.Query().Get("event")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(slog, ctx)

		reqLog.Info("video_event", "event_type", eventType)
		w.WriteHeader(http.StatusNoContent)
	})

	// Audio event
	mux.HandleFunc("/v1/t/audio", func(w http.ResponseWriter, r *http.Request) {
		traceID := r.URL.Query().Get("tid")
		eventType := r.URL.Query().Get("event")

		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(slog, ctx)

		reqLog.Info("audio_event", "event_type", eventType)
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	lc.OnShutdown("http-server", func(ctx context.Context) error {
		return server.Shutdown(ctx)
	})

	slog.Info("tracker starting", "port", port)
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	if err := lc.Wait(30 * time.Second); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}
