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
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestPacingCommittedReflectsBilledNotWins — a win with no impression must
// NOT show up in committed spend (it never bills), and the impression is what
// moves committed. This is the core of the win-vs-impression fix.
func TestPacingCommittedReflectsBilledNotWins(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pacing-committed")
	// Clear the in-memory committed accumulator (it's long-lived and hydrated on
	// boot from campaign_committed_spend, so a prior run's spend would leak in).
	h.ResetBillingLedger(t)
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
	if got := h.CommittedSpendMicros(t, win.CampaignID); got != 0 {
		t.Fatalf("phantom win: committed = %d micro-dollars, want 0 (no impression billed)", got)
	}

	// Now fire the impression (CPM bills immediately) — committed must move to
	// exactly the impression's realized cost: the clearing price is a CPM, so
	// one impression books price/1000 dollars (counters are micro-dollars).
	// Billing consumes off NATS, so poll briefly.
	h.FireImpressionWithModel(t, auc.TraceID,
		win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID,
		"USD", win.Price, "cpm")

	h.WaitCommittedMicros(t, win.CampaignID, harness.Micros(win.Price/1000))
}

// TestPacingReconcileReleasesPhantomWinAtDSP — the DSP end of the loop: phantom
// wins (won, never impressed) over-count the local Redis counter; the
// spend-snapshot reconcile must snap it back to what actually BILLED, so the
// campaign keeps bidding rather than pacing itself out on spend that never
// happened. Reads the DSP counter via /debug/budget (h.DSPSpendMicros).
//
// A pure phantom-only campaign has committed=0 and so isn't in the snapshot map
// (the reconcile only touches campaigns it sees) — so we anchor with ONE real
// impression (committed = its cost) and over-count phantom wins ON TOP; the
// reconcile releases them back down to the single billed impression.
func TestPacingReconcileReleasesPhantomWinAtDSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pacing-dsp-recon")
	h.ResetBillingLedger(t)
	h.SetCampaignBidStrategy(t, w.Campaign, "cpm")
	// Silence the competitors so our internal DSP wins every auction — the win
	// notices must land on the DSP whose /debug/budget we read.
	h.MakeDSPAlwaysNoBid(t, harness.PodDSPCompetitor1)
	h.MakeDSPAlwaysNoBid(t, harness.PodDSPCompetitor2)
	h.RefreshAllCaches(t)

	// One REAL win + impression → billing commits exactly the impression's cost.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "recon-real")
	win := h.ExtractWinner(t, auc)
	if win.NoBid || win.CampaignID != w.Campaign.ID {
		t.Fatalf("expected our internal campaign to win; nobid=%v cid=%s", win.NoBid, win.CampaignID)
	}
	billedMicros := harness.Micros(win.Price / 1000) // clearing price is a CPM
	h.FireImpressionWithModel(t, auc.TraceID,
		win.CampaignID, win.CreativeID, auc.PlacementID, auc.PublisherID, w.AdvAcc.ID,
		"USD", win.Price, "cpm")
	h.WaitCommittedMicros(t, win.CampaignID, billedMicros)

	// Over-count: three PHANTOM wins (won, no impression). The DSP records each
	// win notice, inflating dsp:budget:{campaign}:spent above the one billed imp.
	for i := 0; i < 3; i++ {
		pa := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile",
			fmt.Sprintf("recon-phantom-%d", i))
		if pw := h.ExtractWinner(t, pa); pw.NoBid {
			t.Fatalf("phantom auction %d did not win — cannot over-count the DSP counter", i)
		}
	}

	// Best-effort: observe the over-count (win notices are async; the reporting
	// reconcile ticker is 30s, so we almost always catch it before it reconciles).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.DSPSpendMicros(t, win.CampaignID) > billedMicros {
			t.Logf("observed DSP over-count above billed %d µ (phantom wins recorded)", billedMicros)
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// The reconcile snaps the counter back to what actually BILLED — releasing
	// the phantom over-count.
	h.WaitDSPSpendMicros(t, win.CampaignID, billedMicros)
}
