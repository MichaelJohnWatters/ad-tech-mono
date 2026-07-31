//go:build e2e

// G6 end-to-end: household fallback. The impression's exact user id does NOT
// resolve from the conversion's visitor — only the HOUSEHOLD does. With the ad
// server now baking hh onto the beacon (behaviour_signals.household_id), the
// view-through matcher still finds the exposure via the household key.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAttributionHouseholdFallback(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "g6-hh")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	h.RefreshAllCaches(t)

	suffix := fmt.Sprint(time.Now().UnixNano())
	advUID := "g6-adv-" + suffix       // advertiser visitor id on the conversion
	household := "hh:g6-" + suffix      // the shared household
	deviceUser := "g6-dev-" + suffix    // the device id on the impression (NOT resolvable from advUID)

	// The identity graph links the advertiser visitor to the HOUSEHOLD, not to the
	// device id on the impression — so only a household match can bridge them.
	h.AddIdentityEdge(t, advUID, household, "household")

	settledBefore := summaryFloat(t, h.BillingSummary(t), "TotalSettled")
	reservedBefore := summaryFloat(t, h.BillingSummary(t), "TotalReserved")

	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", deviceUser)
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid")
	}
	// Viewable impression served to the device user WITH the household baked on
	// (what the ad server now does for a consented serve).
	h.FireImpressionWithUserHH(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa", deviceUser, household)
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price/1000)
	if !h.FireView(t, auc.TraceID, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 80, 0) {
		t.Fatal("expected the impression to be IAB-viewable")
	}
	// The behaviour row carries the household id (proves the beacon threaded hh).
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.behaviour_signals WHERE kind='impression' AND household_id='%s' AND account_id='%s'", household, w.AdvAcc.ID),
		"behaviour impression row with household_id")

	// Conversion resolves advUID → household → the exposure (no user-id match).
	convTrace := "order-g6-" + suffix
	h.FireConversionForVisitor(t, convTrace, w.AdvAcc.ID, advUID, "purchase", "USD", 49.99)

	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s' AND attributed_trace_id='%s' AND attribution_type='view_through'", convTrace, auc.TraceID),
		"view-through attributed via household fallback")
}
