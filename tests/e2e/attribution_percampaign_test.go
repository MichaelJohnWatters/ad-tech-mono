//go:build e2e

// G5: per-line-item attribution overrides. A campaign sets a view-through window
// of 0 hours on its targeting_rules row — so a prior viewable exposure that the
// GLOBAL 7-day window would attribute (proven by TestAttributionViewThrough) is
// now OUT of window and does NOT attribute. Proves the per-campaign override is
// read and applied.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAttributionPerCampaignWindow(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "g5-window")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	// Per-campaign override: zero-hour view window → nothing before the conversion
	// qualifies.
	h.SetCampaignAttributionConfig(t, w.Campaign, `{"view_window_hours":0}`)
	h.RefreshAllCaches(t)

	suffix := fmt.Sprint(time.Now().UnixNano())
	advUID := "g5-adv-" + suffix
	pubUser := "g5-pub-" + suffix
	h.AddIdentityEdge(t, advUID, pubUser, "crm_match")

	settledBefore := summaryFloat(t, h.BillingSummary(t), "TotalSettled")
	reservedBefore := summaryFloat(t, h.BillingSummary(t), "TotalReserved")

	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", pubUser)
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid")
	}
	h.FireImpressionWithUser(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa", pubUser)
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price/1000)
	if !h.FireView(t, auc.TraceID, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 80, 0) {
		t.Fatal("expected the impression to be IAB-viewable")
	}
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.behaviour_signals WHERE kind='impression' AND user_id='%s' AND account_id='%s'", pubUser, w.AdvAcc.ID), "behaviour impression row")

	// Conversion names the campaign (cid) so the override is consulted before the
	// lookback.
	convTrace := "order-g5-" + suffix
	h.FireConversionForVisitorCampaign(t, convTrace, w.AdvAcc.ID, win.CampaignID, advUID, "purchase", "USD", 49.99)

	// The zero-hour window excludes the exposure → NOT attributed, NOT settled.
	time.Sleep(2 * time.Second)
	if d := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledBefore; d != 0 {
		t.Errorf("zero-window campaign settled %.6f; want 0 (exposure out of window)", d)
	}
	got := h.ClickHouseScalar(t, fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s' AND attribution_type='view_through'", convTrace))
	if got != 0 {
		t.Errorf("zero-window campaign produced %d view_through attribution rows; want 0", got)
	}
}
