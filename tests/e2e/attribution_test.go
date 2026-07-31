//go:build e2e

// Phase 0 — deterministic click-through attribution closes the trace loop.
//
// The CPA reservation lives on the EARNING exposure's trace (the impression/
// click), but a real advertiser's server-to-server conversion postback carries
// its OWN synthetic order id as the conversion trace. Before Phase 0 that meant
// SettleByTrace looked up a reservation-less trace and nothing settled. Now the
// advertiser returns the earning trace as the signed `ctid`; the tracker stamps
// it as the conversion's AttributedTraceID and reporting settles against it —
// crediting the impression that actually earned the conversion, and the correct
// publisher. This proves that loop end to end over the live stack.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAttributionClickThroughClosesTheLoop(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "attrib-ct")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	h.RefreshAllCaches(t)

	settledBefore := summaryFloat(t, h.BillingSummary(t), "TotalSettled")
	reservedBefore := summaryFloat(t, h.BillingSummary(t), "TotalReserved")

	// Real auction → trace + clearing price come from the exchange.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "attrib-user-1")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the CPA campaign")
	}

	// Impression reserves the bid against the EXPOSURE trace (auc.TraceID).
	h.FireImpressionWithModel(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa")
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price/1000)

	// The user clicks the ad — the touchpoint that earns the credit. In the live
	// flow the tracker's 302 stamps auc.TraceID onto the landing URL, the
	// adtech-adv.js tag captures it, and the advertiser's server returns it as ctid.
	h.FireClick(t, auc.TraceID, win.CampaignID, "https://landing.test/page")

	// 1) Broken path (pre-Phase-0): a conversion carrying ONLY its own synthetic
	//    order trace finds no reservation → must NOT settle. This is exactly the
	//    gap Phase 0 closes.
	orphanTrace := "order-" + fmt.Sprint(time.Now().UnixNano())
	h.FireConversion(t, orphanTrace, win.CampaignID, "purchase", "USD", 49.99)
	time.Sleep(800 * time.Millisecond)
	if d := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledBefore; d != 0 {
		t.Fatalf("unattributed conversion (no ctid) settled %.6f; want 0 — the CPA reservation is on the exposure trace, not the order id", d)
	}

	// 2) Closed loop: same shape, but now the earning trace rides as the signed
	//    ctid. Settlement credits the exposure's CPA reservation.
	convTrace := "order-" + fmt.Sprint(time.Now().UnixNano())
	h.FireConversionAttributed(t, convTrace, auc.TraceID, win.CampaignID, "purchase", "USD", 49.99)
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)

	// The conversions row records the attribution linkage (async NATS → CH).
	linkQ := fmt.Sprintf(
		"SELECT count() FROM adtech.conversions WHERE trace_id='%s' AND attributed_trace_id='%s' AND attribution_type='click_through'",
		convTrace, auc.TraceID)
	deadline := time.Now().Add(12 * time.Second)
	found := 0
	for time.Now().Before(deadline) {
		if found = h.ClickHouseScalar(t, linkQ); found >= 1 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if found < 1 {
		t.Fatalf("conversions row for %s missing attributed_trace_id=%s / attribution_type=click_through", convTrace, auc.TraceID)
	}

	// 3) No double-charge: a retry of the SAME conversion dedups (tracker dedup +
	//    billing HasSettlement guard) → TotalSettled must not move again.
	settledAfter := summaryFloat(t, h.BillingSummary(t), "TotalSettled")
	h.FireConversionAttributed(t, convTrace, auc.TraceID, win.CampaignID, "purchase", "USD", 49.99)
	time.Sleep(800 * time.Millisecond)
	if d := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledAfter; d != 0 {
		t.Errorf("duplicate conversion settled again %.6f; want 0 (dedup + HasSettlement guard)", d)
	}
}
