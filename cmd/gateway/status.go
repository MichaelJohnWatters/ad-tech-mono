package main

// status.go — the PUBLIC status page (PLAN Phase 11, item 107).
//
//	GET /status          — public HTML page (no auth)
//	GET /v1/api/status   — public JSON snapshot (no auth)
//
// It probes each backing service's /readyz, rolls the results up into
// customer-facing components (pkg/statuspage), folds in staff-authored open
// incidents, and reports one overall status. Nothing here is tenant-scoped or
// authenticated — it's the public health face of the platform.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/statuspage"
)

// incidentWindow is how far back resolved incidents show on the public page.
const incidentWindow = 7 * 24 * time.Hour

type statusAggregator struct {
	serviceURLs map[string]string // service name → in-cluster base URL
	components  []statuspage.ComponentDef
	store       statuspage.Store // may be nil (DB down) → no incidents, health still shows
	templates   *templateManager
	probe       *http.Client
	log         *slog.Logger
}

func newStatusAggregator(serviceURLs map[string]string, store statuspage.Store, templates *templateManager, log *slog.Logger) *statusAggregator {
	return &statusAggregator{
		serviceURLs: serviceURLs,
		components:  statuspage.DefaultComponents(),
		store:       store,
		templates:   templates,
		probe:       &http.Client{Timeout: 2 * time.Second},
		log:         log,
	}
}

// report builds the current public snapshot: probe readyz, load incidents, roll up.
func (a *statusAggregator) report(ctx context.Context) statuspage.Report {
	ready := a.probeAll(ctx)
	var incidents []statuspage.Incident
	if a.store != nil {
		if incs, err := a.store.RecentIncidents(ctx, incidentWindow, 20); err != nil {
			a.log.Error("status: load incidents failed", "error", err)
		} else {
			incidents = incs
		}
	}
	return statuspage.Rollup(a.components, ready, incidents, time.Now())
}

// probeAll concurrently hits each backing service's /readyz. The gateway itself
// is always ready (it's serving this request), so it isn't probed.
func (a *statusAggregator) probeAll(ctx context.Context) map[string]bool {
	services := statuspage.AllServices(a.components)
	ready := make(map[string]bool, len(services))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, svc := range services {
		if svc == constants.ServiceGateway {
			mu.Lock()
			ready[svc] = true
			mu.Unlock()
			continue
		}
		url := a.serviceURLs[svc]
		if url == "" {
			continue // unknown target → treated as not-ready
		}
		wg.Add(1)
		go func(svc, url string) {
			defer wg.Done()
			ok := false
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+routes.Readyz, nil)
			if err == nil {
				if resp, err := a.probe.Do(req); err == nil {
					ok = resp.StatusCode == http.StatusOK
					resp.Body.Close()
				}
			}
			mu.Lock()
			ready[svc] = ok
			mu.Unlock()
		}(svc, url)
	}
	wg.Wait()
	return ready
}

// jsonHandler serves GET /v1/api/status (public).
func (a *statusAggregator) jsonHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(a.report(r.Context()))
}

// pageHandler serves GET /status (public HTML).
func (a *statusAggregator) pageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.templates.Render(w, "status.html", a.report(r.Context()))
}
