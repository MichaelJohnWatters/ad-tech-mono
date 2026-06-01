//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// RefreshAllCaches POSTs /debug/cache/refresh on every service that holds a
// warm cache. Synchronous — returns only after each service has reloaded
// from Postgres. Use after any direct-SQL setup step so the next assertion
// sees the cache fully populated, instead of waiting on the 30s poll.
//
// Failure modes:
//   - non-200 from any service: fails the test (the test depends on caches
//     being fresh; silent failures here would just defer the real assertion).
//   - service unreachable: same; WaitReady already passed so this is genuinely
//     unexpected.
func (h *Harness) RefreshAllCaches(t *testing.T) {
	t.Helper()
	urls := []string{
		h.URLs.DSP,
		h.URLs.DSPComp1,
		h.URLs.DSPComp2,
		h.URLs.AdServer,
		h.URLs.SSP,
		h.URLs.Exchange,
		h.URLs.Reporting,
	}
	for _, base := range urls {
		h.refreshOne(t, base)
	}
}

// RefreshCache POSTs the debug endpoint on a single service base URL. Use
// when a test only needs one cache fresh (saves the round-trip on others).
func (h *Harness) RefreshCache(t *testing.T, serviceBaseURL string) {
	t.Helper()
	h.refreshOne(t, serviceBaseURL)
}

type refreshResult struct {
	Refreshed []struct {
		Cache      string `json:"cache"`
		Count      int    `json:"count"`
		DurationMs int64  `json:"duration_ms"`
		Error      string `json:"error,omitempty"`
	} `json:"refreshed"`
}

func (h *Harness) refreshOne(t *testing.T, base string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+routes.DebugCacheRefresh, nil)
	if err != nil {
		t.Fatalf("refresh request build (%s): %v", base, err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("refresh call (%s): %v", base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// 404 is okay only if the service didn't register the endpoint at all —
	// e.g. gateway/tracker which have no warm caches today. We don't include
	// those in the URL list but tolerate the response shape.
	if resp.StatusCode == http.StatusNotFound {
		return
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh (%s) status %d: %s", base, resp.StatusCode, string(body))
	}

	var res refreshResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("refresh (%s) decode: %v\nbody: %s", base, err, string(body))
	}
	for _, r := range res.Refreshed {
		if r.Error != "" {
			t.Fatalf("refresh (%s) cache %q errored: %s", base, r.Cache, r.Error)
		}
		t.Logf("refreshed %s/%s: %d entries in %dms", base, r.Cache, r.Count, r.DurationMs)
	}
}
