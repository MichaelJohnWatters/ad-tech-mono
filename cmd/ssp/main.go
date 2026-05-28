// cmd/ssp is the Supply-Side Platform service.
// Manages publisher inventory, generates bid requests.
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
	slog := logger.New("ssp")
	hlth := health.New()
	lc := lifecycle.New(slog)

	port := cfg.Get("ssp.port", "8084")

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// TODO(phase2-wire): gRPC InventoryService, QualityControlService, DealService
	// TODO(phase2-wire): Bid request generation -> calls Exchange

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	slog.Info("ssp starting", "port", port)
	lifecycle.ServeHTTP(lc, server, slog, 30*time.Second)
}
