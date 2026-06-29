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
		h.URLs.PublisherAdServer,
	}
	for _, base := range urls {
		h.refreshOne(t, base)
	}
	// Audience preloader is a separate mechanism from the warm-cache
	// /debug/cache/refresh path — it has its own ticker that doesn't
	// participate in the warm.RefreshHandler registration. Hit its
	// dedicated endpoint so tests inserting audience_segment_members
	// rows see them on the next bid without waiting for the 30s tick.
	h.RefreshAudiencePreloader(t)
}

// RefreshCache POSTs the debug endpoint on a single service base URL. Use
// when a test only needs one cache fresh (saves the round-trip on others).
func (h *Harness) RefreshCache(t *testing.T, serviceBaseURL string) {
	t.Helper()
	h.refreshOne(t, serviceBaseURL)
}

// RefreshAudiencePreloader forces a synchronous audience preload on both
// DSP and SSP. Use after inserting audience_segment_members rows so the
// bid path sees the new mappings immediately instead of waiting up to
// 30s for the natural preload tick.
//
// Failure modes:
//   - Endpoint missing (audience preloader not active on that service):
//     404 is tolerated silently — that service is in a fallback backend
//     and doesn't need explicit refresh anyway.
//   - Refresh returns 5xx: fails the test loudly. The harness depends on
//     a fresh preloader snapshot; silent failure here would just defer
//     the real assertion failure to an opaque "got NoBid".
func (h *Harness) RefreshAudiencePreloader(t *testing.T) {
	t.Helper()
	for _, base := range []string{h.URLs.DSP, h.URLs.SSP, h.URLs.DSPComp1, h.URLs.DSPComp2} {
		h.audienceRefreshOne(t, base)
	}
}

// ResetBillingLedger wipes the in-memory billing ledger so a test starts
// from zero state. No-op when the reporting service is running with the
// TigerBeetle backend (the debug endpoint returns reset=false and we
// silently accept that). Called from BuildBasicWorld so every test that
// uses the standard fixture gets a clean ledger automatically.
func (h *Harness) ResetBillingLedger(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Reporting+routes.DebugBillingReset, nil)
	if err != nil {
		t.Fatalf("build billing reset request: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("billing reset call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("billing reset status %d: %s", resp.StatusCode, string(body))
	}
}

func (h *Harness) audienceRefreshOne(t *testing.T, base string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+routes.DebugAudienceRefresh, nil)
	if err != nil {
		t.Fatalf("build audience refresh request (%s): %v", base, err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("audience refresh (%s): %v", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Service isn't using the warm preloader backend — no refresh needed.
		return
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("audience refresh (%s) status %d: %s", base, resp.StatusCode, string(body))
	}
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
