//go:build e2e

// Hardening: the attribution resolver must NOT follow a weak (probabilistic)
// identity link when crediting CPA billing. A conversion whose visitor is linked
// to the ad's user ONLY by a low-confidence (0.5) edge — below
// attribution.min_identity_confidence (default 1.0) — must not attribute. (The
// positive case, a deterministic 1.0 link, is TestAttributionViewThrough.)
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAttributionRejectsWeakIdentityLink(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "harden-conf")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	h.RefreshAllCaches(t)

	suffix := fmt.Sprint(time.Now().UnixNano())
	advUID := "hc-adv-" + suffix
	pubUser := "hc-pub-" + suffix
	// WEAK link only (0.5, probabilistic) — below the 1.0 attribution floor.
	h.AddIdentityEdgeWithConfidence(t, advUID, pubUser, "probabilistic", 0.5)

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

	convTrace := "order-hc-" + suffix
	h.FireConversionForVisitor(t, convTrace, w.AdvAcc.ID, advUID, "purchase", "USD", 49.99)

	// The weak link is below the floor → the visitor can't reach the exposure →
	// no attribution, no settle.
	time.Sleep(2 * time.Second)
	if d := summaryFloat(t, h.BillingSummary(t), "TotalSettled") - settledBefore; d != 0 {
		t.Errorf("weak-link conversion settled %.6f; want 0 (below the confidence floor)", d)
	}
	if got := h.ClickHouseScalar(t, fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s' AND attribution_type='view_through'", convTrace)); got != 0 {
		t.Errorf("weak-link conversion produced %d view_through rows; want 0", got)
	}
}
