//go:build e2e

// Campaign targeting depth via the real API: create now exposes include/
// exclude for geo/device/domain/category (was include_geo/include_device
// only). Proves the fields reach the DSP targeting engine and gate bidding —
// a campaign that excludes a geo doesn't bid there but does elsewhere.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCampaignTargetingViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("targ-%d", time.Now().UnixNano())
	// Publisher side (something to auction on).
	pub := h.Signup(t, "Targ Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Targ Site","domain":"`+uniq+`.test"}`)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements",
		fmt.Sprintf(`{"publisher_id":%q,"name":"Targ MPU","format":"display","width":300,"height":250,"floor_price":0.5}`, site["id"]))
	placementID := pl["id"].(string)

	// Advertiser with a campaign that EXCLUDES GBR (all other geos allowed).
	adv := h.Signup(t, "Targ Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"Geo-excluded","base_bid":3.0,"daily_budget":500,
		"exclude_geo":["GBR"]
	}`)
	campaignID := created["id"].(string)

	// Fund the advertiser (prepay gate) — create returns the owning account_id
	// so we avoid a list read that races the warm-cache invalidate.
	advAccountID := created["account_id"].(string)
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")
	h.RefreshAllCaches(t)

	// GBR auction → the excluded geo → no bid.
	if w := h.ExtractWinner(t, h.RunAuction(t, placementID, "GBR", "mobile", "targ-gbr")); !w.NoBid {
		t.Errorf("GBR auction won (%+v), want no-bid (geo excluded)", w)
	}
	// USA auction → allowed → the campaign bids and wins.
	if w := h.ExtractWinner(t, h.RunAuction(t, placementID, "USA", "mobile", "targ-usa")); w.NoBid {
		t.Errorf("USA auction no-bid, want a winning bid (geo not excluded)")
	}

	// EDIT TARGETING via PATCH: move the exclusion from GBR to USA. Now USA
	// should no-bid and GBR should win — proving the campaign PATCH reaches
	// targeting_rules and re-gates the auction.
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID,
		`{"exclude_geo":["USA"]}`)
	h.RefreshAllCaches(t)
	if w := h.ExtractWinner(t, h.RunAuction(t, placementID, "USA", "mobile", "targ-usa-2")); !w.NoBid {
		t.Errorf("after PATCH exclude USA: USA auction won (%+v), want no-bid", w)
	}
	if w := h.ExtractWinner(t, h.RunAuction(t, placementID, "GBR", "mobile", "targ-gbr-2")); w.NoBid {
		t.Errorf("after PATCH: GBR auction no-bid, want a win (GBR no longer excluded)")
	}
}
