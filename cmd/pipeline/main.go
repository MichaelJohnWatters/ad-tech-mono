// cmd/pipeline is the data pipeline service.
// Ingests publisher data files, validates, normalises, and enriches them.
// Watches object storage (Minio/S3) for new files.
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
	log := logger.New("pipeline")
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("pipeline.port", "8087")

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	server := &http.Server{Addr: ":" + port, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("pipeline starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}
