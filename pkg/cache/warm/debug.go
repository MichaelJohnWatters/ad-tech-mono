package warm

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Refreshable is the minimal surface the debug refresh handler needs from
// each cache. Implemented by Cache[T] for any T via Refresh and a
// generic-erased name accessor.
type Refreshable interface {
	Name() string
	Refresh(ctx context.Context) (int, error)
}

// Name returns the cache name from its Config — used by the debug handler
// when reporting which caches were refreshed.
func (c *Cache[T]) Name() string { return c.cfg.Name }

// RefreshHandler returns an HTTP handler that refreshes every passed cache
// synchronously and returns a JSON summary. Used by services to register
// a single /debug/cache/refresh endpoint.
//
// Query param `name` (optional) restricts the refresh to a single named
// cache; absent means refresh everything. The response shape is stable so
// tests and ops tools can parse it the same way across services.
//
// Each cache gets its own 3-second per-Refresh context. Without this,
// a slow Loader (e.g. Postgres briefly under load) could hold the
// handler past the server's WriteTimeout, causing the client to see
// a raw EOF instead of a structured "error" entry in the response. 3
// seconds matches typical postgres query budgets across the platform.
func RefreshHandler(caches ...Refreshable) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("name")
		type result struct {
			Cache      string `json:"cache"`
			Count      int    `json:"count"`
			DurationMs int64  `json:"duration_ms"`
			Error      string `json:"error,omitempty"`
		}
		out := struct {
			Refreshed []result `json:"refreshed"`
		}{}

		anyMatched := false
		for _, c := range caches {
			if filter != "" && c.Name() != filter {
				continue
			}
			anyMatched = true
			start := time.Now()
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			n, err := c.Refresh(ctx)
			cancel()
			res := result{
				Cache:      c.Name(),
				Count:      n,
				DurationMs: time.Since(start).Milliseconds(),
			}
			if err != nil {
				res.Error = err.Error()
			}
			out.Refreshed = append(out.Refreshed, res)
		}

		if filter != "" && !anyMatched {
			http.Error(w, "unknown cache: "+filter, http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
