// cmd/pipeline is the data pipeline service.
// Ingests publisher data files, validates, normalises, and enriches them.
// Watches object storage (Minio/S3) for new files.
package main

import (
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

func main() {
	log := logger.New(constants.ServicePipeline)
	sc := config.Setup(constants.ServicePipeline, keys.PipelineSchema(), log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := keys.Pipeline.Port.Get(cfg)

	// Third-party audience drop-zone: poll the adtech-onboarding bucket for
	// provider CSV files, validate/normalize, write memberships, publish
	// profile_signals over NATS (→ reporting → ClickHouse), quarantine rejects.
	// See onboarding.go. (The Delta dual-write sink + its /debug/datalake/*
	// endpoints were retired in ADR 0006 phase 5 — the lake is now a derived
	// hourly ClickHouse→Parquet export owned by reporting.)
	startOnboarding(cfg, log, lc)

	mux := http.NewServeMux()
	// pprof: the 2026-07-18 wedge was undiagnosable post-mortem because
	// nothing could dump goroutines. Debug surface only (cluster network).
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())

	server := &http.Server{Addr: ":" + port, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("pipeline starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}
