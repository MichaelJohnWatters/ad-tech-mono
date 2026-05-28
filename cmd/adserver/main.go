// cmd/adserver serves ad creatives to end-user browsers.
// Generates tracking URLs with signed parameters.
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

func main() {
	cfg := config.Load()
	slog := logger.New("adserver")
	hlth := health.New()
	lc := lifecycle.New(slog)

	port := cfg.Get("adserver.port", "8085")

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// TODO(phase2-wire): gRPC AdService (ServeAd, GetCreative, UploadCreative, etc.)
	// TODO(phase2-wire): Macro substitution, frequency cap checks, third-party pixels

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	lc.OnShutdown("http-server", func(ctx context.Context) error {
		return server.Shutdown(ctx)
	})

	slog.Info("adserver starting", "port", port)
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
