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
	"fmt"
	"math"
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

	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price/1000)
	// And no settlement should have happened from the impression alone.
	settledMid := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledBefore
	if settledMid != 0 {
		t.Errorf("after impression: TotalSettled delta = %.4f, want 0 (no click yet)", settledMid)
	}

	h.FireClick(t, auc.TraceID, win.CampaignID, "https://landing.test/page")
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)
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
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price/1000)

	// A click should NOT settle CPA — settle only fires on conversion.
	h.FireClick(t, auc.TraceID, win.CampaignID, "https://landing.test/page")
	time.Sleep(500 * time.Millisecond) // give the click event a chance to be (wrongly) settled
	if d := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledBefore; d != 0 {
		t.Errorf("click on CPA campaign should not settle: TotalSettled delta = %.4f", d)
	}

	h.FireConversion(t, auc.TraceID, win.CampaignID, "purchase", "USD", 49.99)
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)
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
// Tolerance is relative (1%) with a tiny absolute floor: post
// money-precision the expected deltas are per-impression amounts
// (~$0.0035), so the old ±0.01 band would have matched a zero delta.
func waitForDelta(t *testing.T, h *harness.Harness, key string, base, expected float64) float64 {
	t.Helper()
	tol := expected * 0.01
	if tol < 0.000005 {
		tol = 0.000005
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		summary := h.BillingSummary(t)
		got := summaryFloat(t, summary, key) - base
		if got >= expected-tol && got <= expected+tol {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	final := summaryFloat(t, h.BillingSummary(t), key) - base
	t.Errorf("%s delta = %.6f, want ~%.6f (5s timeout)", key, final, expected)
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
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price/1000)

	// 2000ms / 75% / no area passes the 50%/1s IAB rule.
	viewable := h.FireView(t, auc.TraceID, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 75, 0)
	if !viewable {
		t.Fatal("expected X-IAB-Viewable=1 for 2000ms / 75%")
	}
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)

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
	waitForDelta(t, h, "TotalReserved", reservedMid, win2.Price/1000)

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
	// The reservation-expiry sweep IS built and unit-tested — Engine.
	// SweepExpiredReservations releases unsettled reserves past the pacing hold
	// TTL, wired into the reporting spend-snapshot tick (see
	// pkg/billing.TestSweepExpiredReservations). It can't be asserted end-to-end
	// here because the LOCAL stack runs billing.ledger_backend=tigerbeetle, whose
	// reservations are PENDING transfers that TB auto-voids server-side after
	// pkg/tb.ReservationTimeoutSeconds (24h) — not runtime-tunable to a test-sized
	// TTL, and the void isn't reflected in the ledger summary the way the memory
	// backend's release is. An e2e version needs either a memory-backend test
	// stack or a configurable TB pending-transfer timeout — a separate effort.
	t.Skip("sweep built + unit-tested (pkg/billing); e2e blocked on the local TigerBeetle backend — see comment")
}

// TestBillingTieredRevenueShareTierFlip — a publisher on a tiered contract earns
// a better split once its month-to-date impressions cross a volume tier. We fire
// past the threshold, refresh the contract cache (which re-reads the per-
// publisher month count from analytics into Contract.MonthImpressions), then a
// fresh impression must book publisher revenue at the tier-2 fee, not tier-1.
func TestBillingTieredRevenueShareTierFlip(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "billing-tier")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpm")
	// Tier 1 (< 5 imps this month): 40% platform fee → publisher keeps 60%.
	// Tier 2 (>= 5): 10% fee → publisher keeps 90%.
	const threshold = 5
	h.SetPublisherContract(t, w.Publisher, "tiered",
		`{"tiers":[{"min_impressions":0,"max_impressions":5,"fee_pct":40},{"min_impressions":5,"fee_pct":10}]}`)
	h.RefreshAllCaches(t)

	fireImp := func(user string) harness.BidResponseWinner {
		auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", user)
		win := h.ExtractWinner(t, auc)
		if win.NoBid {
			t.Fatalf("expected a winning bid for %s", user)
		}
		h.FireImpressionWithModel(t, auc.TraceID, win.CampaignID, win.CreativeID,
			auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpm")
		return win
	}
	pubRev := func() float64 { return summaryFloat(t, h.BillingSummary(t), "TotalPublisherRevenue") }
	// waitStable returns TotalPublisherRevenue once it stops moving (billing
	// consumes impressions async), so a delta measured after it is clean.
	waitStable := func() float64 {
		prev := pubRev()
		stableSince := time.Now()
		for time.Now().Before(stableSince.Add(6 * time.Second)) {
			time.Sleep(400 * time.Millisecond)
			cur := pubRev()
			if cur == prev {
				return cur
			}
			prev = cur
		}
		return prev
	}

	// Cross the threshold — these bill at tier 1 (count not yet refreshed).
	for i := 0; i < threshold+1; i++ {
		fireImp(fmt.Sprintf("tier-cross-%d", i))
	}
	waitStable()

	// Refresh the contract cache so the month count (now > threshold in
	// ClickHouse) flips the publisher to tier 2, then probe with one fresh
	// impression. Retry the refresh+probe: the count query races ClickHouse
	// ingestion of the crossing impressions (and each probe adds one more, so it
	// converges past the threshold).
	deadline := time.Now().Add(30 * time.Second)
	for {
		h.RefreshAllCaches(t) // re-reads ImpressionsByPublisher → SetMonthImpressions
		before := waitStable()
		win := fireImp("tier-probe")
		cost := win.Price / 1000
		// Wait for the probe's revenue to land, then classify the split.
		var delta float64
		moved := time.Now().Add(5 * time.Second)
		for time.Now().Before(moved) {
			if delta = pubRev() - before; delta > 1e-9 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		tier2 := cost * 0.90
		tier1 := cost * 0.60
		if math.Abs(delta-tier2) <= tier2*0.02 {
			return // flipped to tier 2 ✓
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe publisher-revenue delta = %.6f; want tier-2 %.6f (looks like tier-1 %.6f) — tier did not flip",
				delta, tier2, tier1)
		}
		// Still tier 1 (count hadn't crossed in CH yet); loop, refresh, retry.
	}
}

// TestBillingGuaranteedMinimumSubsidy — a publisher on a guaranteed-minimum
// contract earns at least GuaranteedMinCPM (a per-mille rate) even when the
// fee split would leave less. Amounts are per-impression post money-precision:
// with a ~3.50 CPM clearing and a 20% fee the raw publisher share is
// 2.80/1000 = 0.0028 per impression, below the 5.00 CPM floor's 0.005, so the
// platform subsidises up to 0.005 — making the booked publisher-revenue delta
// exactly floor/1000, independent of the exact clearing price (which is why
// this assertion is stable).
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

	waitForDelta(t, h, "TotalPublisherRevenue", before, floor/1000)
}

// TestBillingDealTypeFeeModifier — a deal-won impression bills at the
// contract's deal-type-modified fee, not the open-market fee. Unskipped
// 2026-07-26 after wiring the whole propagation chain it was waiting on:
// SSP now sends DealID in the serve request (it was dropped), the beacon
// carries deal=<id>, and reporting resolves the id to its deal TYPE for
// Contract.DealTypeModifiers (the raw id never matched a type key, so
// deal-won impressions always billed as open market).
func TestBillingDealTypeFeeModifier(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "billing-dealfee")

	// Base fee 20%; PMP deals get -5 → 15% fee → publisher keeps 85%.
	h.SetPublisherContract(t, w.Publisher, "fixed",
		`{"fee_pct":20,"deal_type_modifiers":{"pmp":-5}}`)

	// PMP deal price is a FLOOR the bid must clear (unlike PG's fixed
	// price) — keep it under the DSP's 3.50 base bid so the bid qualifies
	// and the deal preempts the open market on priority.
	const dealPrice = 2.00
	dealID := h.CreateDeal(t, w.Publisher, "e2e-fee-pmp", "pmp", dealPrice,
		[]string{w.AdvAcc.ID}, []string{w.Placement.ID})
	h.RefreshAllCaches(t)

	before := summaryFloat(t, h.BillingSummary(t), "TotalPublisherRevenue")

	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "dealfee-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the PMP-deal placement")
	}
	if win.DealID != dealID {
		t.Fatalf("winner deal = %q, want the PMP deal %q (deal must preempt open market)", win.DealID, dealID)
	}

	h.FireImpressionDeal(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, win.DealID)

	// 15% fee, not 20%: publisher revenue = price × 0.85 per mille. A result
	// of price × 0.80 means the deal type never reached billing.
	waitForDelta(t, h, "TotalPublisherRevenue", before, (win.Price/1000)*0.85)
}

// TestBillingCurrencyConversion — a non-USD impression books the
// USD-converted amount (exchange_rates is the truth), and an unknown
// currency is REFUSED (no booking) rather than treated as dollars.
// Unskipped 2026-07-26: the exchange_rates table had zero consumers until
// billing.SetRateSource landed — the "feature" this test waited on never
// actually existed.
func TestBillingCurrencyConversion(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "billing-eur")

	var rate float64
	if err := h.DB.QueryRow(`
SELECT rate FROM exchange_rates
WHERE base_currency='USD' AND target_currency='EUR' AND effective_date <= CURRENT_DATE
ORDER BY effective_date DESC LIMIT 1`).Scan(&rate); err != nil || rate <= 0 {
		t.Fatalf("EUR rate not seeded (err=%v rate=%v) — run make seed", err, rate)
	}

	before := summaryFloat(t, h.BillingSummary(t), "TotalSpend")

	// EUR impression: the clearing CPM is treated as EUR and lands as USD.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "eur-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid for the EUR conversion case")
	}
	h.FireImpression(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "EUR", win.Price)
	wantDelta := (win.Price / 1000) / rate
	waitForDelta(t, h, "TotalSpend", before, wantDelta)

	// Unknown currency: refused up front, TotalSpend must NOT move again.
	afterEUR := summaryFloat(t, h.BillingSummary(t), "TotalSpend")
	auc2 := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "eur-user-2")
	win2 := h.ExtractWinner(t, auc2)
	if win2.NoBid {
		t.Fatal("expected a winning bid for the unknown-currency case")
	}
	h.FireImpression(t, auc2.TraceID, win2.CampaignID, win2.CreativeID,
		auc2.PlacementID, auc2.PublisherID, w.AdvAcc.ID, "ZZZ", win2.Price)
	time.Sleep(3 * time.Second) // give the async chain time to (not) book it
	if got := summaryFloat(t, h.BillingSummary(t), "TotalSpend"); got > afterEUR+0.000001 {
		t.Errorf("unknown-currency impression moved TotalSpend %v -> %v; must be refused, not booked", afterEUR, got)
	}
}
