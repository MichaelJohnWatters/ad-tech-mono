//go:build e2e

// Phase 2 increment 2 — the identity bridge is BUILT by the pixel, not seeded.
//
// The view-through test seeds the advertiser_uid ↔ publisher_user edge directly.
// Here the advertiser side of that bridge is created for real: the retargeting
// pixel carries the advertiser visitor id + a hashed email, and the tracker
// publishes an identity observation so the identity-consumer writes the
// advertiser_uid ↔ hashed_email edge. Combined with the publisher-side
// hashed_email ↔ publisher_user edge (what the SSP observes on serve), a
// click-less conversion then resolves advertiser_uid → hashed_email →
// publisher_user → the viewable impression, and settles. End to end, no seeded
// advertiser edge.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAttributionIdentityBridgeFromPixel(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "attrib-bridge")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	h.RefreshAllCaches(t)

	// Unique per run so persisted edges don't collide across runs.
	suffix := fmt.Sprint(time.Now().UnixNano())
	advUID := "br-adv-" + suffix  // advertiser first-party visitor id
	he := "br-he-" + suffix       // shared hashed email (the bridge key)
	pubUser := "br-pub-" + suffix // publisher-side user id

	// Publisher half of the bridge: the SSP observed this hashed email alongside
	// the publisher user when serving (seeded here). The ADVERTISER half is NOT
	// seeded — the pixel below must create it.
	h.AddIdentityEdge(t, he, pubUser, "hashed_email")

	// Fire the retargeting pixel with the visitor id + hashed email. The tracker
	// publishes an identity observation → identity-consumer writes advUID ↔ he.
	h.FireRetargetingPixel(t, w.AdvAcc.ID, advUID, he, "bridge-tag")

	// Prove the pixel created the advertiser edge (identity-consumer is async).
	deadline := time.Now().Add(25 * time.Second)
	for h.IdentityEdgeCount(t, advUID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("retargeting pixel did not create the advertiser_uid↔hashed_email identity edge")
		}
		time.Sleep(500 * time.Millisecond)
	}

	settledBefore := summaryFloat(t, h.BillingSummary(t), "TotalSettled")
	reservedBefore := summaryFloat(t, h.BillingSummary(t), "TotalReserved")

	// A viewable CPA impression served to the publisher user.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", pubUser)
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid on the CPA campaign")
	}
	h.FireImpressionWithUser(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa", pubUser)
	waitForDelta(t, h, "TotalReserved", reservedBefore, win.Price/1000)
	if !h.FireView(t, auc.TraceID, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 80, 0) {
		t.Fatal("expected the impression to be IAB-viewable")
	}
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.behaviour_signals WHERE kind='impression' AND user_id='%s' AND account_id='%s'", pubUser, w.AdvAcc.ID), "behaviour impression row")
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.views WHERE trace_id='%s' AND iab_viewable=1", auc.TraceID), "viewable view row")

	// Click-less conversion carrying only the advertiser visitor id. Attribution
	// resolves advUID → he → pubUser across the PIXEL-created + publisher edges.
	convTrace := "order-" + suffix
	h.FireConversionForVisitor(t, convTrace, w.AdvAcc.ID, advUID, "purchase", "USD", 49.99)

	waitForDelta(t, h, "TotalSettled", settledBefore, win.Price/1000)
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s' AND attributed_trace_id='%s' AND attribution_type='view_through'", convTrace, auc.TraceID),
		"view-through attributed conversion via the pixel-built bridge")
}
