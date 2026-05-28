// cmd/adserver serves ad creatives to end-user browsers.
// Generates tracking URLs with signed parameters.
package main

import (
	"net/http"
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

	slog.Info("adserver starting", "port", port)
	lifecycle.ServeHTTP(lc, server, slog, 30*time.Second)
}
