//go:build e2e

// Profile Store Phase 2 — consent-gated behavioural signal capture.
//
// A consented ad request produces a behaviour_signals lake row keyed to the
// user (SSP request row, categories stamped at event time); a GPC "do not
// sell/share" request produces none. Verified per-user via the pipeline's
// residual endpoint, so a busy stack can't false-positive the assertion.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func waitBehaviourRows(t *testing.T, h *harness.Harness, userID string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if n := h.SignalResidual(t, userID)["behaviour_signals"]; n > 0 {
			return n
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(2 * time.Second)
	}
}

func TestBehaviourSignalsCaptured(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "behaviour-sig")
	uniq := time.Now().UnixNano()

	// Consented request (no regulatory signals = personalisation allowed).
	consented := fmt.Sprintf("bhv-ok-%d", uniq)
	h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "USA", Device: "mobile", UserID: consented,
	})
	if n := waitBehaviourRows(t, h, consented, 45*time.Second); n == 0 {
		t.Error("consented request produced no behaviour_signals row")
	}

	// GPC do-not-sell/share → capture suppressed.
	gpcUser := fmt.Sprintf("bhv-gpc-%d", uniq)
	h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "USA", Device: "mobile", UserID: gpcUser, GPC: "1",
	})
	// Give the pipeline the same landing window the positive case needed,
	// then assert nothing arrived. (The positive wait above already proves
	// the pipeline is consuming, so 15s here isn't racing a cold consumer.)
	time.Sleep(15 * time.Second)
	if n := h.SignalResidual(t, gpcUser)["behaviour_signals"]; n != 0 {
		t.Errorf("GPC request produced %d behaviour_signals rows, want 0", n)
	}
}
