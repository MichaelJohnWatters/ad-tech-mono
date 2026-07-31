//go:build e2e

// Phase 3 — multi-touch attribution (reporting-only). A user is exposed to the
// SAME campaign THREE times before converting. Attribution records the whole
// chain (attribution_touchpoints), so fractional credit can be computed per
// model on read — but BILLING is unchanged: exactly one last-touch settle. This
// proves the chain is captured and the money side stays last-touch.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAttributionMultiTouchChain(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "attrib-mta")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	h.RefreshAllCaches(t)

	const pubUser = "mta-user-1" // conversion uid == exposure user (self-match, no bridge needed)

	settledBefore := summaryFloat(t, h.BillingSummary(t), "TotalSettled")

	// One auction gives a realistic campaign/creative/price; then THREE viewable
	// impressions to the same user on distinct traces (fabricated so no per-user
	// auction freq-cap interferes) — each reserves CPA + writes a behaviour row.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", pubUser)
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the CPA campaign")
	}
	traces := []string{auc.TraceID + "-mta1", auc.TraceID + "-mta2", auc.TraceID + "-mta3"}
	for _, tr := range traces {
		h.FireImpressionWithUser(t, tr, win.CampaignID, win.CreativeID,
			auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa", pubUser)
		if !h.FireView(t, tr, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 80, 0) {
			t.Fatalf("impression %s should be IAB-viewable", tr)
		}
		time.Sleep(120 * time.Millisecond) // distinct observed_at ordering; last fired = last touch
	}
	lastTouch := traces[len(traces)-1]

	// Wait until all three viewable exposures are queryable.
	waitCHCount(t, h, fmt.Sprintf("SELECT count() FROM adtech.behaviour_signals WHERE kind='impression' AND user_id='%s' AND account_id='%s'", pubUser, w.AdvAcc.ID), 3, "3 behaviour impression rows")

	// Click-less conversion for the same user.
	convTrace := "order-mta-" + fmt.Sprint(time.Now().UnixNano())
	h.FireConversionForVisitor(t, convTrace, w.AdvAcc.ID, pubUser, "purchase", "USD", 49.99)

	// Billing is unchanged: exactly ONE last-touch settle (the most-recent
	// exposure), not three. Same price on all three, so 2+ settles would blow the
	// 1% band.
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)

	// The conversion is credited to the most-recent exposure (last touch).
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s' AND attributed_trace_id='%s' AND attribution_type='view_through'", convTrace, lastTouch),
		"last-touch attributed conversion")

	// But the FULL chain of 3 touchpoints is recorded for multi-touch reporting.
	waitCHCount(t, h, fmt.Sprintf("SELECT count() FROM adtech.attribution_touchpoints WHERE conversion_trace_id='%s'", convTrace), 3, "3-touchpoint attribution chain")

	// The reporting API apportions credit per model on read: linear splits the 3
	// exposures evenly (~1/3 each, summing to 1).
	b := h.GetAttribution(t, convTrace, "linear")
	if len(b.Touchpoints) != 3 {
		t.Fatalf("attribution API: got %d touchpoints, want 3", len(b.Touchpoints))
	}
	var sum float64
	for _, tp := range b.Touchpoints {
		if tp.CreditFraction < 0.32 || tp.CreditFraction > 0.34 {
			t.Errorf("linear credit = %.4f, want ~0.333 (%s)", tp.CreditFraction, tp.TraceID)
		}
		sum += tp.CreditFraction
	}
	if sum < 0.999 || sum > 1.001 {
		t.Errorf("credit fractions sum to %.4f, want 1", sum)
	}
	// last_touch gives all credit to the most-recent exposure.
	lt := h.GetAttribution(t, convTrace, "last_touch")
	if lt.Touchpoints[len(lt.Touchpoints)-1].CreditFraction != 1 {
		t.Errorf("last_touch: newest should get 1.0, got %+v", lt.Touchpoints)
	}
}

// waitCHCount polls until a COUNT query returns at least `want`, failing at 15s.
func waitCHCount(t *testing.T, h *harness.Harness, query string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.ClickHouseScalar(t, query) >= want {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (want >= %d)", what, want)
}
