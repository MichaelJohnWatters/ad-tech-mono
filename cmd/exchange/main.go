// cmd/exchange is the Ad Exchange service.
// Receives bid requests, fans out to DSPs, runs auctions.
//
// Deployment: one instance per channel in prod, --channel=all locally.
//
//	cmd/exchange --channel=display
//	cmd/exchange --channel=all
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

func main() {
	clk := clock.Real{}
	cfg := config.Load()
	slog := logger.New("exchange")
	hlth := health.New()
	lc := lifecycle.New(slog)

	port := cfg.Get("exchange.port", "8081")
	channel := cfg.Get("exchange.channel", "all")
	_ = cfg.GetDuration("exchange.bid_timeout", 100*time.Millisecond)

	engine := auction.NewEngine(clk)

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// OpenRTB auction endpoint
	mux.HandleFunc("/v1/openrtb/auction", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var bidReq openrtb.BidRequest
		if err := json.NewDecoder(r.Body).Decode(&bidReq); err != nil {
			http.Error(w, "invalid bid request", http.StatusBadRequest)
			return
		}

		traceID := bidReq.ID
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(slog, ctx)

		reqLog.Info("auction started", "channel", channel)

		// TODO(phase2-wire): Fan out to DSPs, collect bids, run auction
		_ = engine

		resp := openrtb.BidResponse{ID: bidReq.ID, NoBid: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("auction complete", "result", "no_fill")
	})

	mux.HandleFunc("/v1/openrtb/win", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/v1/openrtb/loss", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	lc.OnShutdown("http-server", func(ctx context.Context) error {
		return server.Shutdown(ctx)
	})

	slog.Info("exchange starting", "port", port, "channel", channel)
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
