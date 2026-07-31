//go:build e2e

// Follow-up to Phase 3: a CLICK-THROUGH conversion also records its assisting
// exposures in the multi-touch chain (previously only view-through did). The
// click stays the last touch (what settles); prior impressions for the same
// user are captured as assists.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAttributionClickThroughChain(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "attrib-ctchain")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	h.RefreshAllCaches(t)

	const user = "ctchain-user-1"
	settledBefore := summaryFloat(t, h.BillingSummary(t), "TotalSettled")

	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", user)
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the CPA campaign")
	}

	// An earlier ASSIST impression for the user (viewable), on its own trace.
	assist := auc.TraceID + "-assist"
	h.FireImpressionWithUser(t, assist, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa", user)
	h.FireView(t, assist, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 80, 0)
	time.Sleep(150 * time.Millisecond)

	// The EARNING impression/click (the ctid) — also an exposure for the user.
	earn := auc.TraceID + "-earn"
	h.FireImpressionWithUser(t, earn, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa", user)
	h.FireView(t, earn, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 80, 0)
	h.FireClick(t, earn, win.CampaignID, "https://landing.test/page")
	waitCHCount(t, h, fmt.Sprintf("SELECT count() FROM adtech.behaviour_signals WHERE kind='impression' AND user_id='%s' AND account_id='%s'", user, w.AdvAcc.ID), 2, "2 behaviour impression rows")

	// Click-through conversion: ctid = the earning trace, carrying the user id so
	// its assists can be resolved.
	convTrace := "order-ctchain-" + fmt.Sprint(time.Now().UnixNano())
	h.FireConversionAttributedUser(t, convTrace, earn, w.AdvAcc.ID, user, "purchase", "USD", 49.99)

	// Settles click-through against the earning trace.
	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s' AND attributed_trace_id='%s' AND attribution_type='click_through'", convTrace, earn),
		"click-through attributed conversion")

	// And BOTH exposures (assist + earning) are recorded in the chain.
	waitCHCount(t, h, fmt.Sprintf("SELECT count() FROM adtech.attribution_touchpoints WHERE conversion_trace_id='%s'", convTrace), 2, "click-through assist chain")
}
