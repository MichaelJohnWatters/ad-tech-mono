//go:build e2e

// Deal type matrix — one test per priority tier + the edge cases (time
// window, paused). PG is covered by step 10 in functionality_test.go.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestDealPreferredApplies — a Preferred deal that matches the bidding
// advertiser should set DealID on the winning bid (and effective floor
// up to deal.price), but not preempt the auction the way PG does.
func TestDealPreferredApplies(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "preferred")

	// Lift our bid well above competitor noise so we win reliably — only
	// then does our bid's DealID reach the winner.
	h.SetCampaignBaseBid(t, w.Campaign, harness.OverbidCompetitors)

	// Preferred at 2.00, below base bid → bid passes floor, wins, DealID populated.
	dealID := h.CreateDeal(t, w.Publisher, "deal-preferred",
		"preferred", 2.00,
		[]string{w.AdvAcc.ID},
		[]string{w.Placement.ID},
	)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "pref-user-1")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected winning bid for Preferred deal, got no_bid")
	}
	if win.DealID != dealID {
		t.Errorf("winner DealID = %q, want %q (Preferred deal)", win.DealID, dealID)
	}
	if win.Price < 3.50 {
		t.Errorf("clearing price = %.4f, expected >= 3.50 (base bid)", win.Price)
	}
}

// TestDealPMPAllowlistExcludesOthers — a PMP deal that lists only some
// other advertiser must NOT apply when our advertiser bids. Auction falls
// back to open (no DealID).
func TestDealPMPAllowlistExcludesOthers(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pmp-other")

	// Allowlist points at a different (synthetic) advertiser account.
	stranger := h.CreateAdvertiser(t, "pmp-stranger-adv")
	h.CreateDeal(t, w.Publisher, "deal-pmp-other",
		"pmp", 100.00, // very high price so if it WAS applied, the bid would fall under it
		[]string{stranger.ID},
		[]string{w.Placement.ID},
	)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "pmp-user-1")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected winning bid (PMP deal excludes our advertiser → open auction), got no_bid")
	}
	if win.DealID != "" {
		t.Errorf("winner DealID = %q, want empty (PMP not applicable to our advertiser)", win.DealID)
	}
}

// TestDealPMPAllowlistAdmitsListed — control case: same setup as the
// excludes test, but the bidding advertiser IS on the allowlist → deal
// applies. Both tests in one suite verify the gate works both ways.
func TestDealPMPAllowlistAdmitsListed(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pmp-listed")

	// Lift our bid above competitor noise so the deal manifests on the winner.
	h.SetCampaignBaseBid(t, w.Campaign, harness.OverbidCompetitors)

	dealID := h.CreateDeal(t, w.Publisher, "deal-pmp-listed",
		"pmp", 2.00,
		[]string{w.AdvAcc.ID},
		[]string{w.Placement.ID},
	)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "pmp-user-2")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected winning bid for listed advertiser, got no_bid")
	}
	if win.DealID != dealID {
		t.Errorf("winner DealID = %q, want %q (PMP should apply)", win.DealID, dealID)
	}
}

// TestDealTimeWindowFutureNotActive — a deal whose start_date is tomorrow
// must not match. Without this filter, a draft deal scheduled for next
// week would silently price-floor every auction starting now.
func TestDealTimeWindowFutureNotActive(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "deal-future")

	dealID := h.CreateDeal(t, w.Publisher, "deal-future",
		"pg", 100.00, // high price so a "should not apply" failure would noticeably preempt
		[]string{w.AdvAcc.ID},
		[]string{w.Placement.ID},
	)
	// Move the start date forward by 7 days.
	future := time.Now().AddDate(0, 0, 7)
	h.SetDealDates(t, dealID, w.PubAcc.ID, &future, nil)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "deal-future-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected open-auction win (future-dated deal not active), got no_bid")
	}
	if win.DealID == dealID {
		t.Errorf("future-dated deal %q was incorrectly applied to the auction", dealID)
	}
}

// TestDealTimeWindowPastNotActive — same as future but end_date in the past.
// Expired deals must be excluded.
func TestDealTimeWindowPastNotActive(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "deal-past")

	dealID := h.CreateDeal(t, w.Publisher, "deal-past",
		"pg", 100.00,
		[]string{w.AdvAcc.ID},
		[]string{w.Placement.ID},
	)
	past := time.Now().AddDate(0, 0, -1)
	pastStart := past.AddDate(0, 0, -30)
	h.SetDealDates(t, dealID, w.PubAcc.ID, &pastStart, &past)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "deal-past-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected open-auction win (expired deal not active), got no_bid")
	}
	if win.DealID == dealID {
		t.Errorf("expired deal %q was incorrectly applied", dealID)
	}
}

// TestDealPausedExcluded — paused deal must not match even when otherwise
// eligible. status='active' is the only state the loader accepts.
func TestDealPausedExcluded(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "deal-paused")

	dealID := h.CreateDeal(t, w.Publisher, "deal-paused",
		"pg", 100.00,
		[]string{w.AdvAcc.ID},
		[]string{w.Placement.ID},
	)
	h.SetDealStatus(t, dealID, w.PubAcc.ID, "paused")
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "deal-paused-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected open-auction win (paused deal not active), got no_bid")
	}
	if win.DealID == dealID {
		t.Errorf("paused deal %q was incorrectly applied", dealID)
	}
}
