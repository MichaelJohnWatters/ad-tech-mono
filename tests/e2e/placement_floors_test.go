//go:build e2e

// Placement floor overrides via the real API: floor_config device/geo floors
// are now resolved per-request by the SSP (pkg/floors) and set as the bid
// request's floor — the exchange drops bids below it. Proves a geo floor
// gates bidding: a campaign that clears the base floor still no-bids where a
// high geo override applies.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestPlacementFloorOverridesViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("floors-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Floors Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Floors Site","domain":"`+uniq+`.test"}`)
	// Base floor 0.50, but USA is gated to a 10.00 CPM override.
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Floors MPU","format":"display","width":300,"height":250,
		"floor_price":0.50, "floor_config":{"geo":{"USA":10.0}}
	}`, site["id"]))
	placementID := pl["id"].(string)

	// A campaign that bids 3.00 — above the 0.50 base, well below the USA 10.00.
	adv := h.Signup(t, "Floors Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{"name":"Floors","base_bid":3.0,"daily_budget":500}`)
	advAccountID := created["account_id"].(string)
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")
	h.RefreshAllCaches(t)

	// USA: the 10.00 geo floor applies → the 3.00 bid is below it → no win.
	if w := h.ExtractWinner(t, h.RunAuction(t, placementID, "USA", "mobile", "floors-usa")); !w.NoBid {
		t.Errorf("USA auction won at %.2f, want no-bid (below the 10.00 geo floor)", w.Price)
	}
	// GBR: only the 0.50 base floor applies → the 3.00 bid clears it → wins.
	if w := h.ExtractWinner(t, h.RunAuction(t, placementID, "GBR", "mobile", "floors-gbr")); w.NoBid {
		t.Errorf("GBR auction no-bid, want a win (3.00 clears the 0.50 base floor)")
	}

	// Read-back: the list returns the floor_config so the portal can edit it.
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/placements", nil)
	resp, _ := pub.Do(req)
	var pls []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&pls)
	resp.Body.Close()
	for _, p := range pls {
		if p["ID"] == placementID {
			fc, _ := p["FloorConfig"].(map[string]any)
			geo, _ := fc["geo"].(map[string]any)
			if geo["USA"] != 10.0 {
				t.Errorf("floor_config geo.USA = %v, want 10", geo["USA"])
			}
		}
	}
}
