// cmd/gateway is the API Gateway and dashboard server.
// Single entry point for all REST/dashboard traffic.
// Proxies to internal gRPC services.
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
	slog := logger.New("gateway")
	hlth := health.New()
	lc := lifecycle.New(slog)

	port := cfg.Get("gateway.port", "8080")

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// Dashboard home
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Ad Tech Platform</title></head>
<body>
<h1>Ad Tech Platform</h1>
<p>Dashboard coming in Phase 2.</p>
<ul>
<li><a href="/healthz">/healthz</a> - liveness</li>
<li><a href="/readyz">/readyz</a> - readiness</li>
<li><a href="/docs">/docs</a> - API docs (coming soon)</li>
</ul>
</body>
</html>`))
	})

	// TODO(phase2-wire): Auth middleware (JWT/RBAC)
	// TODO(phase2-wire): gRPC proxying to DSP, SSP, Reporting, AdServer
	// TODO(phase2-wire): HTMX dashboard templates (role-gated)
	// TODO(phase2-wire): Swagger UI at /docs

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	lc.OnShutdown("http-server", func(ctx context.Context) error {
		return server.Shutdown(ctx)
	})

	slog.Info("gateway starting", "port", port)
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
