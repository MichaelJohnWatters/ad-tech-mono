//go:build e2e

// Gaps G1 (publisher-side identity bridge from the serve) and G3 (the attribution
// read endpoint is tenant-scoped).
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// G1: a serve carrying the publisher user id + a hashed email (what adtech.js now
// forwards via setUserData) makes the SSP observe both, so the identity-consumer
// writes the publisher half of the cross-site bridge — no seeding.
func TestIdentityBridgePublisherSideFromServe(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "g1-bridge")
	h.RefreshAllCaches(t)

	suffix := fmt.Sprint(time.Now().UnixNano())
	pubUser := "g1-pub-" + suffix
	he := "g1-he-" + suffix

	h.ServePubAdRaw(t, fmt.Sprintf("placement_id=%s&user_id=%s&hashed_email=%s&geo=USA&device=mobile",
		w.Placement.ExternalID, pubUser, he))

	// The SSP→identity-consumer write is async.
	deadline := time.Now().Add(25 * time.Second)
	for h.IdentityEdgeCount(t, pubUser) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("serve with hashed_email did not create the publisher_user↔hashed_email identity edge")
		}
		time.Sleep(500 * time.Millisecond)
	}
	// The edge touches the hashed email too.
	if h.IdentityEdgeCount(t, he) == 0 {
		t.Errorf("expected an identity edge touching the hashed email %q", he)
	}
}

// G3: the attribution read endpoint only returns a conversion's chain to its
// owning advertiser (or staff). Another advertiser gets an empty chain; an
// unscoped caller is forbidden.
func TestAttributionEndpointTenantScoped(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "g3-scope")
	h.SetCampaignBidStrategy(t, w.Campaign, "cpa")
	h.RefreshAllCaches(t)

	// Produce a view-through conversion for advertiser w.AdvAcc → an
	// attribution_touchpoints chain scoped to that account.
	suffix := fmt.Sprint(time.Now().UnixNano())
	advUID := "g3-adv-" + suffix
	pubUser := "g3-pub-" + suffix
	h.AddIdentityEdge(t, advUID, pubUser, "crm_match")

	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", pubUser)
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid")
	}
	h.FireImpressionWithUser(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, "cpa", pubUser)
	h.FireView(t, auc.TraceID, win.CampaignID, auc.PlacementID, auc.PublisherID, 2000, 80, 0)
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.behaviour_signals WHERE kind='impression' AND user_id='%s' AND account_id='%s'", pubUser, w.AdvAcc.ID), "behaviour impression row")

	convTrace := "order-g3-" + suffix
	h.FireConversionForVisitor(t, convTrace, w.AdvAcc.ID, advUID, "purchase", "USD", 49.99)
	waitCHCount(t, h, fmt.Sprintf("SELECT count() FROM adtech.attribution_touchpoints WHERE conversion_trace_id='%s'", convTrace), 1, "attribution chain")

	// Owner (advertiser) sees the chain.
	owner := h.GetAttributionAs(t, convTrace, "linear", "advertiser", w.AdvAcc.ID)
	if len(owner.Touchpoints) == 0 {
		t.Error("owning advertiser should see its own conversion chain")
	}
	// A DIFFERENT advertiser gets an empty chain (no leak).
	other := h.GetAttributionAs(t, convTrace, "linear", "advertiser", "some-other-advertiser-id")
	if len(other.Touchpoints) != 0 {
		t.Errorf("another advertiser must NOT see the chain; got %d touchpoints", len(other.Touchpoints))
	}
	// Staff is unscoped.
	if len(h.GetAttribution(t, convTrace, "linear").Touchpoints) == 0 {
		t.Error("staff should see the chain (unscoped)")
	}
	// No scope header → forbidden.
	if s := h.AttributionStatusAs(t, convTrace, "", ""); s != http.StatusForbidden {
		t.Errorf("unscoped request status = %d, want 403", s)
	}
}
