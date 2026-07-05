//go:build e2e

// Campaign targeting depth via the real API: create now exposes include/
// exclude for geo/device/domain/category (was include_geo/include_device
// only). Proves the fields reach the DSP targeting engine and gate bidding —
// a campaign that excludes a geo doesn't bid there but does elsewhere.
package e2e

import (
	"encoding/json"
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

	// Fund the advertiser (prepay gate) — pull its account id from the list.
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/campaigns", nil)
	resp, _ := adv.Do(req)
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	var advAccountID string
	for _, c := range list {
		if c["ID"] == campaignID {
			advAccountID = c["AccountID"].(string)
		}
	}
	if advAccountID == "" {
		t.Fatalf("campaign %s not found in list", campaignID)
	}
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
}
