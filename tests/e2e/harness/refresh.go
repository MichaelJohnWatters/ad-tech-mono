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
	// Blanket ads.txt authorisation: under the prod-shaped strict enforcement
	// (exchange.adstxt_enforcement=strict), every publisher must be listed in
	// ads_txt_cache or its auctions no-bid "adstxt_not_authorised". Authorise
	// EVERY publisher in the DB here — regardless of how it was created
	// (AddPublisher, the API, the seed, big-world) — right before the exchange
	// reloads its ads.txt warm cache below. Idempotent; ads_txt_cache is global
	// (no RLS). This is the single point that keeps the whole e2e suite green
	// under strict without touching every world builder.
	h.authorizeAllPublishers(t)
	urls := []string{
		h.URLs.DSP,
		h.URLs.DSPComp1,
		h.URLs.DSPComp2,
		h.URLs.AdServer,
		h.URLs.SSP,
		h.URLs.Exchange,
		h.URLs.Reporting,
		h.URLs.PublisherAdServer,
		h.URLs.Tracker, // fraud blocklist warm cache (IP/UA blocklists)
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

// authorizeAllPublishers writes the authorising ads.txt line for every
// publisher domain in the DB (matching the exchange's seller identity
// adtech.local / adtech-exchange), so strict ads.txt enforcement lets legit
// auctions through. The cmd/adstxt crawler does this in prod from real ads.txt
// files; the harness writes it directly. Idempotent.
func (h *Harness) authorizeAllPublishers(t *testing.T) {
	t.Helper()
	const entries = `[{"Domain":"adtech.local","AccountID":"adtech-exchange","Relationship":"DIRECT"}]`
	// DO NOTHING (not DO UPDATE): only authorise publishers that have no ads.txt
	// row yet. A test that set a CUSTOM authorisation/seller identity for its
	// domain (e.g. fraud ads.txt tests using adtech.example/seat-1) must not be
	// clobbered back to the default here. Reset truncates ads_txt_cache and
	// domains are unique per world, so there's no stale cross-test carryover.
	if _, err := h.DB.Exec(`
INSERT INTO ads_txt_cache (domain, entries, status, last_fetched, last_changed)
SELECT DISTINCT domain, $1::jsonb, 'valid', now(), now() FROM publishers WHERE domain <> ''
ON CONFLICT (domain) DO NOTHING`,
		entries); err != nil {
		t.Fatalf("authorize all publishers ads.txt: %v", err)
	}
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
	// Pipeline FIRST: it hosts the single append-based cache writer, so draining
	// it makes the Redis sets fresh; the DSP/SSP refreshes are legacy no-ops now
	// (they read the sets the writer maintains).
	for _, base := range []string{h.URLs.Pipeline, h.URLs.DSP, h.URLs.SSP, h.URLs.DSPComp1, h.URLs.DSPComp2} {
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
	// Same port-forward-flap retry as refreshOne: a transient EOF on the
	// first POST must not fail the test.
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = h.HTTP.Post(h.URLs.Reporting+routes.DebugBillingReset, "application/json", nil)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatalf("billing reset call after retries: %v", err)
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

// getWithRetry issues a GET with the same port-forward-flap retry as
// refreshOne: the tunnel to a service flaps (EOF / connection reset) when a
// pod is briefly busy under full-suite CPU load or the port-forward
// reconnects, and a single-shot GET would fast-fail a test's SETUP (before
// its real WaitFor even runs). GET is idempotent, so re-sending is safe.
// h.HTTP's own timeout bounds each attempt. Returns the last error after 3
// transport failures; the caller still checks the status code.
func (h *Harness) getWithRetry(url string) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		resp, err = h.HTTP.Get(url)
		if err == nil {
			return resp, nil
		}
	}
	return resp, err
}

func (h *Harness) refreshOne(t *testing.T, base string) {
	t.Helper()
	// Retry transport errors: the Tilt port-forwards flap (EOF / connection
	// reset) when a pod restarts or the tunnel reconnects — a transient that
	// used to fail whole tests on the very first refresh POST. h.HTTP's own
	// 10s timeout bounds each attempt.
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = h.HTTP.Post(base+routes.DebugCacheRefresh, "application/json", nil)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatalf("refresh call (%s) after retries: %v", base, err)
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
