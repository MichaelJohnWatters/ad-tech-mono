// Package health provides /healthz and /readyz HTTP endpoints for K8s probes.
//
// Every service registers its dependencies (Postgres, Redis, NATS, etc.)
// and the health package exposes endpoints that K8s uses for liveness
// and readiness probes.
//
// Usage:
//
//	h := health.New()
//	h.AddReadinessCheck("postgres", func(ctx context.Context) error {
//	    return db.PingContext(ctx)
//	})
//	h.AddReadinessCheck("nats", func(ctx context.Context) error {
//	    return natsConn.Status() == nats.CONNECTED
//	})
//	mux.Handle("/healthz", h.LivenessHandler())
//	mux.Handle("/readyz", h.ReadinessHandler())
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Check is a function that returns nil if the dependency is healthy.
type Check func(ctx context.Context) error

// Health manages liveness and readiness checks.
type Health struct {
	mu              sync.RWMutex
	readinessChecks map[string]Check
}

// New creates a new Health instance.
func New() *Health {
	return &Health{
		readinessChecks: make(map[string]Check),
	}
}

// AddReadinessCheck registers a named readiness check.
// The service is "ready" only when ALL registered checks pass.
func (h *Health) AddReadinessCheck(name string, check Check) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.readinessChecks[name] = check
}

// LivenessHandler returns an HTTP handler for /healthz.
// Liveness is simple: if the process can respond, it's alive.
func (h *Health) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
	})
}

// ReadinessHandler returns an HTTP handler for /readyz.
// Readiness checks all registered dependencies. If any fail,
// the service is not ready to receive traffic.
func (h *Health) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		h.mu.RLock()
		checks := make(map[string]Check, len(h.readinessChecks))
		for k, v := range h.readinessChecks {
			checks[k] = v
		}
		h.mu.RUnlock()

		results := make(map[string]string, len(checks))
		allHealthy := true

		for name, check := range checks {
			if err := check(ctx); err != nil {
				results[name] = err.Error()
				allHealthy = false
			} else {
				results[name] = "ok"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if allHealthy {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		json.NewEncoder(w).Encode(map[string]any{
			"status": map[bool]string{true: "ready", false: "not_ready"}[allHealthy],
			"checks": results,
		})
	})
}
