//go:build e2e

// Pacing reconciliation tests.
//
// The DSP's local budget counter is decremented on the win NOTICE (nurl),
// which over-counts against actual billing: a win that never renders an
// impression still decrements pacing, and CPC/CPA count the full clearing
// price on the win though spend only bills on the click/conversion. The
// billing engine is the single source of truth — it tracks per-campaign
// COMMITTED spend (settled + open reserves) and reporting broadcasts it on
// adtech.billing.campaign_spend_snapshot; every DSP reconciles its pacing
// counter to that.
//
// These tests verify the billing-side committed figure through the real
// stack: a phantom win (no impression) commits nothing; an impression is
// what commits spend. That committed figure is exactly what DSPs reconcile
// to, so proving it here proves the number the DSP pacing snaps to.
package e2e

import (
	"math"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func cents(price float64) int64 { return int64(math.Round(price * 100)) }

// TestPacingCommittedReflectsBilledNotWins — a win with no impression must
// NOT show up in committed spend (it never bills), and the impression is what
// moves committed. This is the core of the win-vs-impression fix.
func TestPacingCommittedReflectsBilledNotWins(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pacing-committed")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpm")
	h.RefreshAllCaches(t)

	// Win an auction but fire NO impression — a phantom win. The DSP's local
	// counter increments on the win notice, but billing sees nothing.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "pacing-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid")
	}

	// Committed spend for a freshly-built campaign with no impression is 0:
	// the phantom win contributed nothing to billing.
	if got := h.CommittedSpendCents(t, win.CampaignID); got != 0 {
		t.Fatalf("phantom win: committed = %d cents, want 0 (no impression billed)", got)
	}

	// Now fire the impression (CPM bills immediately) — committed must move to
	// exactly the clearing price. Billing consumes off NATS, so poll briefly.
	h.FireImpressionWithModel(t, auc.TraceID,
		win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID,
		"USD", win.Price, "cpm")

	h.WaitCommittedCents(t, win.CampaignID, cents(win.Price))
}

// TestPacingReconcileReleasesPhantomWinAtDSP — the DSP end of the loop: after
// a phantom win over-counts the local Redis counter, a forced snapshot should
// reconcile it back down (committed=0), so the campaign keeps bidding rather
// than pacing itself out on spend that never billed.
//
// SKIPPED pending a DSP spend-read helper: asserting the reconcile effect
// needs to read the DSP's dsp:budget:{campaign}:spent counter (or a
// /debug/budget endpoint on the DSP). The billing-side number the DSP
// reconciles to is already proven by TestPacingCommittedReflectsBilledNotWins;
// the DSP-side Reconcile overwrite is unit-tested in
// cmd/dsp/budget_test.go (TestBudgetTracker_ReconcileOverwrites). Flip this to
// an assertion when h.DSPSpendCents(campaign) lands.
func TestPacingReconcileReleasesPhantomWinAtDSP(t *testing.T) {
	t.Skip("pending harness.DSPSpendCents / DSP /debug/budget read endpoint")
}
