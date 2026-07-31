//go:build e2e

// Phase 2 — view-through attribution. A user SEES an ad (viewable impression, no
// click), then later converts on the advertiser's site. The conversion carries
// only the advertiser's first-party visitor id — a different id space from the
// publisher-side user id on the impression. Attribution bridges them through the
// identity graph, finds the prior viewable exposure within the window, and
// credits + settles the CPA conversion against it. Proven end to end.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAttributionViewThrough(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "attrib-vt")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	h.RefreshAllCaches(t)

	const pubUser = "vt-pubuser-1"  // publisher-side id on the impression
	const advUID = "vt-advuid-1"    // advertiser-side visitor id on the conversion
	// The bridge: the identity graph links the two id spaces (as a shared hashed
	// email would). Without this edge the conversion can't reach the exposure.
	h.AddIdentityEdge(t, advUID, pubUser, "crm_match")

	settledBefore := summaryFloat(t, h.BillingSummary(t), "TotalSettled")
	reservedBefore := summaryFloat(t, h.BillingSummary(t), "TotalReserved")

	// Real auction → a CPA impression served to pubUser, reserved on trace T.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", pubUser)
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the CPA campaign")
	}
	h.FireImpressionWithUser(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa", pubUser)
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price/1000)

	// The impression is VIEWABLE (2s / 80% passes the IAB display rule).
	if !h.FireView(t, auc.TraceID, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 80, 0) {
		t.Fatal("expected the impression to be IAB-viewable")
	}

	// Wait for the async writes the lookback needs: the behaviour_signals
	// impression row (carries user_id + advertiser account_id) and the viewable
	// views row.
	impQ := fmt.Sprintf("SELECT count() FROM adtech.behaviour_signals WHERE kind='impression' AND user_id='%s' AND account_id='%s' AND trace_id='%s'",
		pubUser, w.AdvAcc.ID, auc.TraceID)
	viewQ := fmt.Sprintf("SELECT count() FROM adtech.views WHERE trace_id='%s' AND iab_viewable=1", auc.TraceID)
	waitCH(t, h, impQ, "behaviour_signals impression row")
	waitCH(t, h, viewQ, "viewable views row")

	// The conversion: click-less, carrying only the advertiser account + visitor
	// id (NO ctid). Attribution must resolve advUID → pubUser → the exposure.
	convTrace := "order-" + fmt.Sprint(time.Now().UnixNano())
	h.FireConversionForVisitor(t, convTrace, w.AdvAcc.ID, advUID, "purchase", "USD", 49.99)

	// It settles CPA against the earning IMPRESSION (trace T).
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)

	// And the conversion row records the view-through linkage.
	linkQ := fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s' AND attributed_trace_id='%s' AND attribution_type='view_through'",
		convTrace, auc.TraceID)
	waitCH(t, h, linkQ, "view-through attributed conversion row")

	// Negative control: a conversion for an UNKNOWN visitor (no identity edge, no
	// prior exposure) must not attribute and must not settle.
	settledAfter := summaryFloat(t, h.BillingSummary(t), "TotalSettled")
	h.FireConversionForVisitor(t, "order-"+fmt.Sprint(time.Now().UnixNano()),
		w.AdvAcc.ID, "vt-unknown-visitor", "purchase", "USD", 49.99)
	time.Sleep(1500 * time.Millisecond)
	if d := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledAfter; d != 0 {
		t.Errorf("conversion for an unknown visitor settled %.6f; want 0 (no exposure to attribute)", d)
	}
}

// waitCH polls a COUNT query until it returns >=1, failing after 15s.
func waitCH(t *testing.T, h *harness.Harness, query, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.ClickHouseScalar(t, query) >= 1 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
