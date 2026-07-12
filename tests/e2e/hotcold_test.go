//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestHotColdStore is a LONG-RUNNING test (it waits out the reporting
// hot_window on purpose). It proves the hot/cold HotColdStore end-to-end against
// the live stack:
//
//  1. fire a known burst -> reporting (hot/ClickHouse) reports it, and the
//     pipeline writes it to the cold lake (dual-write);
//  2. wait until the burst ages past hot_window -> the same query is now served
//     ENTIRELY from the cold lake (delta_scan) and must still be lossless;
//  3. fire fresh traffic -> a boundary-spanning query merges cold + fresh-hot
//     exactly.
//
// Runs only against a hot/cold storage-enabled stack (duckdb reporting + pipeline). On the
// default/memory e2e stack it skips. Use `make test-e2e-hotcold`.
func TestHotColdStore(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	// Guard: only meaningful with hot/cold storage on — otherwise every query is hot-only
	// and a passing count would prove nothing.
	if v, ok := h.ReportingEnvVar(t, "REPORTING_COLD_STORE_ENABLED"); !ok || v != "true" {
		t.Skip("hot/cold storage not enabled (REPORTING_COLD_STORE_ENABLED != true) — run `make test-e2e-hotcold`")
	}
	hwStr, _ := h.ReportingEnvVar(t, "REPORTING_HOT_WINDOW")
	hotWindow, err := time.ParseDuration(hwStr)
	if err != nil || hotWindow <= 0 {
		t.Skipf("could not read a usable REPORTING_HOT_WINDOW (%q): %v", hwStr, err)
	}
	t.Logf("hot_window=%s — this test waits it out (long-running by design)", hotWindow)

	w := harness.BuildBasicWorld(t, h, "hot/cold storage") // resets state
	from := time.Now().UTC().Add(-5 * time.Second)
	lakeBefore := h.LakeRows(t, "impressions") // lake is cumulative across runs

	// --- Phase 1: fire a known burst (timestamped ~now -> HOT) ---
	const N = 30
	fired := fireImpressions(t, h, w, "cold", N)
	if fired == 0 {
		t.Fatal("no impressions fired — serving path broken")
	}
	t.Logf("phase 1: fired %d impressions", fired)

	// Hot baseline: the hot/cold query (data still fresh) == fired, from ClickHouse.
	harness.WaitFor(t, 30*time.Second, "hot count to reach fired", func() bool {
		return h.ReportImpressionCountSince(t, from) >= fired
	})
	if got := h.ReportImpressionCountSince(t, from); got != fired {
		t.Fatalf("hot count = %d, want %d", got, fired)
	}
	// Cold write path received it too (dual-write): lake grew by >= fired.
	harness.WaitFor(t, 30*time.Second, "lake to receive the burst", func() bool {
		return h.LakeRows(t, "impressions")-lakeBefore >= fired
	})

	// --- Phase 2: wait out hot_window; the burst becomes COLD ---
	wait := hotWindow + 25*time.Second
	t.Logf("phase 2: waiting %s for the burst to age past the boundary...", wait)
	time.Sleep(wait)

	// [from,now] now routes entirely to the lake (delta_scan) — must be lossless.
	if got := h.ReportImpressionCountSince(t, from); got != fired {
		t.Fatalf("COLD read = %d, want %d (lossless cold read failed / degraded to empty hot)", got, fired)
	}
	// Proof it came from cold, not hot: the recent hot window is empty.
	recent := time.Now().UTC().Add(-hotWindow / 3)
	if got := h.ReportImpressionCountSince(t, recent); got != 0 {
		t.Fatalf("recent (hot) count = %d, want 0 — so the %d must have come from cold", got, fired)
	}
	t.Logf("phase 2: cold read returned %d losslessly; recent hot window empty ✓", fired)

	// --- Phase 3: fresh traffic; boundary-spanning query must MERGE ---
	freshFrom := time.Now().UTC().Add(-2 * time.Second)
	const M = 12
	freshFired := fireImpressions(t, h, w, "merge", M)
	if freshFired == 0 {
		t.Fatal("no fresh impressions fired")
	}
	harness.WaitFor(t, 30*time.Second, "merge count to reach cold+fresh", func() bool {
		return h.ReportImpressionCountSince(t, from) >= fired+freshFired
	})
	if got := h.ReportImpressionCountSince(t, from); got != fired+freshFired {
		t.Fatalf("MERGE count = %d, want %d (cold %d + fresh %d)", got, fired+freshFired, fired, freshFired)
	}
	// The fresh burst is served from HOT (recent window == fresh).
	if got := h.ReportImpressionCountSince(t, freshFrom); got != freshFired {
		t.Fatalf("fresh HOT count = %d, want %d", got, freshFired)
	}
	t.Logf("PASS: cold %d + fresh hot %d = %d merged exactly across the boundary", fired, freshFired, fired+freshFired)
}

// fireImpressions runs n auctions on the world's placement (unique users to
// dodge frequency caps) and fires an impression for each win. Returns the number
// actually fired (== wins), so assertions are robust to any no-bids.
func fireImpressions(t *testing.T, h *harness.Harness, w harness.World, tag string, n int) int {
	t.Helper()
	fired := 0
	for i := 0; i < n; i++ {
		user := fmt.Sprintf("hc-%s-%d", tag, i)
		res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", user)
		win := h.ExtractWinner(t, res)
		if win.NoBid {
			continue
		}
		h.FireImpression(t, res.TraceID,
			win.CampaignID, win.CreativeID,
			w.Placement.ID, w.Publisher.ID, w.AdvAcc.ID,
			"USD", win.Price,
		)
		fired++
	}
	return fired
}
