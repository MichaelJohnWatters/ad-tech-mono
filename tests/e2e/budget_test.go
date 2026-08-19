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
	// Fresh committed accumulator: the DSP's budget tracker keeps spend in memory
	// keyed by campaign, and BuildBasicWorld reuses a deterministic campaign id
	// across runs — so without this a re-run (or prior suite spend on this
	// campaign) starts already over the tiny $0.005 cap and auction 1 no-bids.
	h.ResetBillingLedger(t)

	// Shrink the budget so only a couple of auctions exhaust it. Bid is 3.50
	// CPM, which books 3.50/1000 = $0.0035 per winning impression; a budget of
	// $0.005 means the second win pushes spend over the cap and the third
	// auction must NoBid.
	h.SetCampaignDailyBudget(t, w.Campaign, 0.005)
	h.RefreshAllCaches(t)

	// First auction: our campaign must bid and win. Poll rather than asserting
	// once — the just-issued ledger reset + budget update propagate to the DSP's
	// warm spend mirror on its 1s refresh, so for a beat the DSP can still see a
	// prior run's exhausted spend and no-bid. Break on the first win (spend
	// 0 → ~$0.0035); while it's showing exhausted it no-bids and accrues nothing,
	// so polling can't over-spend.
	var win1 harness.BidResponseWinner
	harness.WaitFor(t, 15*time.Second, "auction 1: our campaign wins (budget available)", func() bool {
		res1 := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "budget-user-001")
		win1 = h.ExtractWinner(t, res1)
		return !win1.NoBid && win1.CampaignID == w.Campaign.ID
	})

	// Win notifications from the exchange to the DSP are async. Allow a
	// brief settle window before the next auction sees the updated spend.
	// SmartRouter / NATS publish run in goroutines, so does the win-notice
	// HTTP call back to the DSP. 500ms is plenty for localhost.
	time.Sleep(500 * time.Millisecond)

	// Second auction: still under budget (spend $0.0035 < $0.005), bids and wins.
	// Spend goes to ~$0.007 — over the cap.
	res2 := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "budget-user-002")
	win2 := h.ExtractWinner(t, res2)
	if win2.NoBid {
		t.Fatal("auction 2 unexpectedly no_bid before budget exhausted")
	}

	// Third auction: internal DSP's spend (~$0.007) >= budget ($0.005), so our
	// campaign drops out of the internal DSP's bid candidates. Competitor
	// DSPs (dsp-competitor1/2) keep bidding on this placement from their
	// own campaigns, so the auction still returns a winner — but the
	// winning seat is no longer our test advertiser.
	//
	// The drop is NOT instant: the win-notice (exchange → DSP, async) and the
	// DSP's budget gate both read a warm in-process spend mirror refreshed every
	// dsp.bid_cache_refresh_interval (1s). A fixed 500ms sleep raced that refresh
	// (worse under full-suite load), so our advertiser could still be winning
	// auction 3 for a beat — the recurring flake here. Poll until the budget
	// exclusion has propagated and our advertiser is no longer the winning seat.
	// Each extra auction only fires more no-bid demand from our (exhausted) DSP,
	// so it can't un-exhaust the budget.
	var win3 harness.BidResponseWinner
	harness.WaitFor(t, 15*time.Second, "budget-exhausted advertiser drops out of auction 3", func() bool {
		res3 := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "budget-user-003")
		win3 = h.ExtractWinner(t, res3)
		return win3.NoBid || win3.Seat != w.AdvAcc.ID
	})
}
