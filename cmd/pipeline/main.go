// cmd/pipeline is the data pipeline service.
// Ingests publisher data files, validates, normalises, and enriches them.
// Watches object storage (Minio/S3) for new files.
package main

import (
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func main() {
	log := logger.New(constants.ServicePipeline)
	sc := config.Setup(constants.ServicePipeline, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("pipeline.port", routes.PortPipeline)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())

	server := &http.Server{Addr: ":" + port, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("pipeline starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}
