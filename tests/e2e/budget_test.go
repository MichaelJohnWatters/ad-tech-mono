//go:build e2e

// Budget cap regression test.
//
// The DSP's bid handler skips campaigns whose tracked spend >= daily_budget.
// Spend is tracked in Redis via atomic IncrBy keyed by the campaign UUID;
// the exchange notifies the DSP via /v1/openrtb/win after each auction
// which is what drives the counter up. After enough winning auctions the
// campaign must stop bidding.
//
// This is financial integrity — over-spend is the worst class of bug.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestBudgetCap(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "budget")

	// Shrink the budget so only a couple of auctions exhaust it. Bid is
	// 3.50 base; budget of 5.00 means the second win pushes spend over the
	// cap, and the third auction must NoBid.
	h.SetCampaignDailyBudget(t, w.Campaign, 5.00)
	h.RefreshAllCaches(t)

	// First auction: campaign bids and wins. Spend goes from 0 → ~3.50.
	res1 := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "budget-user-001")
	win1 := h.ExtractWinner(t, res1)
	if win1.NoBid {
		t.Fatal("auction 1 unexpectedly no_bid before budget exhausted")
	}
	if win1.CampaignID != w.Campaign.ID {
		t.Fatalf("auction 1 winning campaign = %q, want %q", win1.CampaignID, w.Campaign.ID)
	}

	// Win notifications from the exchange to the DSP are async. Allow a
	// brief settle window before the next auction sees the updated spend.
	// SmartRouter / NATS publish run in goroutines, so does the win-notice
	// HTTP call back to the DSP. 500ms is plenty for localhost.
	time.Sleep(500 * time.Millisecond)

	// Second auction: still under budget (spend 3.50 < 5.00), bids and wins.
	// Spend goes to ~7.00 — over the cap.
	res2 := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "budget-user-002")
	win2 := h.ExtractWinner(t, res2)
	if win2.NoBid {
		t.Fatal("auction 2 unexpectedly no_bid before budget exhausted")
	}

	time.Sleep(500 * time.Millisecond)

	// Third auction: internal DSP's spend (~7.00) >= budget (5.00), so our
	// campaign drops out of the internal DSP's bid candidates. Competitor
	// DSPs (dsp-competitor1/2) keep bidding on this placement from their
	// own campaigns, so the auction still returns a winner — but the
	// winning seat is no longer our test advertiser. Verify that.
	res3 := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "budget-user-003")
	win3 := h.ExtractWinner(t, res3)
	if !win3.NoBid && win3.Seat == w.AdvAcc.ID {
		t.Errorf("auction 3: our advertiser %q kept winning despite budget exhaustion (price=%.4f)",
			w.AdvAcc.ID, win3.Price)
	}
}
