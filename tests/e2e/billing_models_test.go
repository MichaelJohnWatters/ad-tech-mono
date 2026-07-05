//go:build e2e

// Billing model tests — verify the reserve/settle pattern works for CPC
// and CPA. Both models reserve on impression and settle on the trigger
// event (click for CPC, conversion for CPA). The reservation is what
// accrues to the ledger.
//
// The pkg/billing engine supports reserve/settle end-to-end. The
// cmd/reporting consumer dispatches via Engine.SettleByTrace on click +
// conversion events; the reservation row carries enough of the original
// auction context (clearing price, bid model, deal type) that the
// settle-side event payload doesn't need it.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestBillingCPCReserveAndSettle — impression on a CPC campaign reserves
// the bid price against the advertiser's budget. The corresponding click
// settles the reservation (publisher revenue + platform margin are split
// at settle time). Asserts deltas because the in-memory ledger
// accumulates across the whole test suite.
func TestBillingCPCReserveAndSettle(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "billing-cpc")
	const bidModel = "cpc"
	h.SetCampaignBidStrategy(t, w.Campaign, bidModel)
	h.RefreshAllCaches(t)

	before := h.BillingSummary(t)
	reservedBefore := summaryFloat(t, before, "TotalReserved")
	settledBefore := summaryFloat(t, before, "TotalSettled")

	// Run a real auction so trace_id + clearing price come from the
	// exchange rather than test-fabricated values.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "cpc-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the CPC campaign")
	}

	// Fire impression — should create a reservation, not a spend entry.
	h.FireImpressionWithModel(t, auc.TraceID,
		win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID,
		"USD", win.Price, bidModel)

	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price)
	// And no settlement should have happened from the impression alone.
	settledMid := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledBefore
	if settledMid != 0 {
		t.Errorf("after impression: TotalSettled delta = %.4f, want 0 (no click yet)", settledMid)
	}

	h.FireClick(t, auc.TraceID, win.CampaignID, "https://landing.test/page")
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price)
}

// TestBillingCPAReserveAndSettle — same flow as CPC but settle fires on
// conversion, not click. Verifies that a click on a CPA campaign is
// recorded as analytics but does NOT settle the reservation.
func TestBillingCPAReserveAndSettle(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "billing-cpa")
	const bidModel = "cpa"
	h.SetCampaignBidStrategy(t, w.Campaign, bidModel)
	h.RefreshAllCaches(t)

	before := h.BillingSummary(t)
	reservedBefore := summaryFloat(t, before, "TotalReserved")
	settledBefore := summaryFloat(t, before, "TotalSettled")

	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "cpa-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the CPA campaign")
	}

	h.FireImpressionWithModel(t, auc.TraceID,
		win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID,
		"USD", win.Price, bidModel)
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price)

	// A click should NOT settle CPA — settle only fires on conversion.
	h.FireClick(t, auc.TraceID, win.CampaignID, "https://landing.test/page")
	time.Sleep(500 * time.Millisecond) // give the click event a chance to be (wrongly) settled
	if d := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledBefore; d != 0 {
		t.Errorf("click on CPA campaign should not settle: TotalSettled delta = %.4f", d)
	}

	h.FireConversion(t, auc.TraceID, win.CampaignID, "purchase", "USD", 49.99)
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price)
}

func summaryFloat(t *testing.T, s map[string]any, key string) float64 {
	t.Helper()
	v, ok := s[key].(float64)
	if !ok {
		t.Fatalf("billing summary missing %s (float64); got %#v", key, s[key])
	}
	return v
}

// waitForDelta polls the billing summary up to 5s for the named key's
// value to differ from `base` by approximately `expected`. NATS publish
// is async — the tracker returns before the impression event lands in
// the reporting consumer, and a fixed sleep races on a busy box.
func waitForDelta(t *testing.T, h *harness.Harness, key string, base, expected float64) float64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		summary := h.BillingSummary(t)
		got := summaryFloat(t, summary, key) - base
		if got >= expected-0.01 && got <= expected+0.01 {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	final := summaryFloat(t, h.BillingSummary(t), key) - base
	t.Errorf("%s delta = %.4f, want ~%.4f (5s timeout)", key, final, expected)
	return final
}

// TestBillingViewabilityVCPMSettle — impression on a vCPM campaign
// reserves the bid price; a viewable view event settles it. A
// non-viewable view does NOT settle (the reservation stays open until
// the expiry cron runs, which isn't built yet — see
// TestBillingReservationExpiry). Matches the platform's "viewable or
// nothing" default contract semantic (decision #3 in docs/PLAN.md →
// "vCPM Settlement Model").
func TestBillingViewabilityVCPMSettle(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "billing-vcpm")
	const bidModel = "vcpm"
	h.SetCampaignBidStrategy(t, w.Campaign, bidModel)
	h.RefreshAllCaches(t)

	before := h.BillingSummary(t)
	reservedBefore := summaryFloat(t, before, "TotalReserved")
	settledBefore := summaryFloat(t, before, "TotalSettled")

	// 1) Viewable path — impression reserves, view settles.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "vcpm-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the vCPM campaign")
	}

	h.FireImpressionWithModel(t, auc.TraceID,
		win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID,
		"USD", win.Price, bidModel)
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price)

	// 2000ms / 75% / no area passes the 50%/1s IAB rule.
	viewable := h.FireView(t, auc.TraceID, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 75, 0)
	if !viewable {
		t.Fatal("expected X-IAB-Viewable=1 for 2000ms / 75%")
	}
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price)

	// 2) Non-viewable path — impression reserves but view does NOT settle.
	settledMid := summaryFloat(t, h.BillingSummary(t), "TotalSettled")
	reservedMid := summaryFloat(t, h.BillingSummary(t), "TotalReserved")

	auc2 := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "vcpm-user-2")
	win2 := h.ExtractWinner(t, auc2)
	if win2.NoBid {
		t.Fatal("expected a winning bid on the second auction")
	}
	h.FireImpressionWithModel(t, auc2.TraceID,
		win2.CampaignID, win2.CreativeID,
		auc2.PlacementID, auc2.PublisherID, w.AdvAcc.ID,
		"USD", win2.Price, bidModel)
	waitForDelta(t, h, "TotalReserved", reservedMid, win2.Price)

	// 500ms is below the 1s IAB threshold → not viewable.
	notViewable := h.FireView(t, auc2.TraceID, win2.CampaignID, auc2.PlacementID, auc2.PublisherID, 500, 100, 0)
	if notViewable {
		t.Fatal("500ms should not be IAB viewable")
	}
	time.Sleep(500 * time.Millisecond) // give the (no-op) settle a chance to incorrectly fire
	if d := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledMid; d != 0 {
		t.Errorf("non-viewable view should not settle: TotalSettled delta = %.4f", d)
	}
}

func TestBillingReservationExpiry(t *testing.T) {
	t.Skip("reservation expiry needs a cron helper (RunRollup-like) plus a low TTL config knob; pending")
}

func TestBillingTieredRevenueShareTierFlip(t *testing.T) {
	t.Skip("contract-write helper now exists (harness.SetPublisherContract for tiers); still pending: the tier is chosen off Contract.MonthImpressions, which the settle path must populate from the publisher's running impression count — verify that wiring + use FireNAuctions to cross a tier, then flip.")
}

// TestBillingGuaranteedMinimumSubsidy — a publisher on a guaranteed-minimum
// contract earns at least GuaranteedMinCPM per impression even when the fee
// split would leave less. With clearing ~3.50 and a 20% fee the raw publisher
// share is 2.80, below the 5.00 floor, so the platform subsidises up to 5.00 —
// making the booked publisher-revenue delta exactly the floor, independent of
// the exact clearing price (which is why this assertion is stable).
func TestBillingGuaranteedMinimumSubsidy(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "billing-gmin")

	const floor = 5.00
	h.SetPublisherContract(t, w.Publisher, "guaranteed_minimum",
		`{"fee_pct":20,"guaranteed_min_cpm":5.0}`)
	h.RefreshAllCaches(t)

	before := summaryFloat(t, h.BillingSummary(t), "TotalPublisherRevenue")

	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "gmin-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the guaranteed-minimum publisher")
	}
	// Sanity: the raw split must be below the floor for the subsidy to engage.
	if win.Price*0.8 >= floor {
		t.Fatalf("clearing %.2f × 0.8 = %.2f is not below the %.2f floor; test needs a lower bid",
			win.Price, win.Price*0.8, floor)
	}

	// CPM books publisher revenue on the impression; the guaranteed minimum
	// clamps it up to the floor.
	h.FireImpression(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price)

	waitForDelta(t, h, "TotalPublisherRevenue", before, floor)
}

func TestBillingDealTypeFeeModifier(t *testing.T) {
	t.Skip("contract-write helper now exists (harness.SetPublisherContract for deal_type_modifiers); still pending a fixture that lands a deal-won impression AND confirms deal_type propagates from the impression event into the billing SpendEvent — flip once that path is verified on a live stack.")
}

func TestBillingCurrencyConversion(t *testing.T) {
	t.Skip("multi-currency flow needs exchange_rates table seeded + a non-USD campaign; pending")
}
