// cmd/gateway is the API Gateway and dashboard server.
// Single entry point for all REST/dashboard traffic.
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
	log := logger.New("gateway")
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("gateway.port", "8080")

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// Static files (CSS, JS, adtech.js)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	// Publisher Simulator pages
	mux.HandleFunc("/dev/publisher-simulator", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/templates/simulator/minimal.html")
	})
	mux.HandleFunc("/dev/publisher-simulator/minimal", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/templates/simulator/minimal.html")
	})

	// Dashboard home
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Ad Tech Platform</title>
<style>
body { font-family: -apple-system, sans-serif; max-width: 600px; margin: 40px auto; padding: 0 20px; color: #333; }
h1 { color: #1a1a2e; }
a { color: #4361ee; }
ul { line-height: 2; }
.status { background: #e8faf0; padding: 12px; border-radius: 6px; margin: 16px 0; border-left: 4px solid #4ade80; }
</style>
</head>
<body>
<h1>Ad Tech Platform</h1>
<div class="status">Phase 2: Core ad serving is live. First auction completed end-to-end.</div>
<h3>Developer Tools</h3>
<ul>
<li><a href="/dev/publisher-simulator">Publisher Simulator</a> - see ads rendering with debug overlay</li>
<li><a href="/healthz">/healthz</a> - liveness probe</li>
<li><a href="/readyz">/readyz</a> - readiness probe</li>
</ul>
<h3>Services</h3>
<ul>
<li>Gateway: :8080 (this page)</li>
<li>Exchange: :8081 (OpenRTB auctions)</li>
<li>DSP: :8082 (bid evaluation)</li>
<li>Tracker: :8083 (event pixels)</li>
<li>SSP: :8084 (inventory management)</li>
<li>Ad Server: :8085 (creative serving)</li>
</ul>
<h3>Quick Test</h3>
<pre>go run ./cmd/simulator single --geo GBR --device mobile</pre>
</body>
</html>`))
	})

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	lc.OnShutdown("http-server", func(ctx context.Context) error {
		return server.Shutdown(ctx)
	})

	log.Info("gateway starting", "port", port)
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	if err := lc.Wait(30 * time.Second); err != nil {
		log.Error("shutdown error", "error", err)
	}
}
