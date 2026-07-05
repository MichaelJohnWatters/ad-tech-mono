//go:build e2e

// Daypart (time-based) floor overrides via the real API: floor_config.dayparts
// raise the per-request floor during matching time windows, resolved by the
// SSP (pkg/floors) against the request time in the config's timezone. Proves a
// currently-active daypart gates bidding while a non-matching one is ignored.
//
// Determinism: both placements pin timezone "UTC" and the test computes "now"
// in UTC, so the assertion holds regardless of the SSP pod's local timezone.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDaypartFloorsViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("daypart-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Daypart Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Daypart Site","domain":"`+uniq+`.test"}`)
	siteID := site["id"].(string)

	// Placement A: an all-day (00:00–24:00, every weekday) 10.00 daypart floor —
	// always active → the 3.00 bid falls below it.
	active := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Daypart Active","format":"display","width":300,"height":250,
		"floor_price":0.50,
		"floor_config":{"timezone":"UTC","dayparts":[{"days":[0,1,2,3,4,5,6],"start_hour":0,"end_hour":24,"floor":10.0}]}
	}`, siteID))
	activeID := active["id"].(string)

	// Placement B: a 1-hour window starting 3 hours from now (UTC) — guaranteed
	// NOT active during this run → only the 0.50 base floor applies.
	s := (time.Now().UTC().Hour() + 3) % 24
	e := (s + 1) % 24
	inactive := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Daypart Inactive","format":"display","width":300,"height":250,
		"floor_price":0.50,
		"floor_config":{"timezone":"UTC","dayparts":[{"start_hour":%d,"end_hour":%d,"floor":10.0}]}
	}`, siteID, s, e))
	inactiveID := inactive["id"].(string)

	// A campaign bidding 3.00 — above the 0.50 base, below the 10.00 daypart floor.
	adv := h.Signup(t, "Daypart Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{"name":"Daypart","base_bid":3.0,"daily_budget":500}`)
	campaignID := created["id"].(string)
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
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")
	h.RefreshAllCaches(t)

	// Active daypart: the 10.00 floor applies now → the 3.00 bid is below it → no win.
	if w := h.ExtractWinner(t, h.RunAuction(t, activeID, "USA", "mobile", "daypart-active")); !w.NoBid {
		t.Errorf("active-daypart auction won at %.2f, want no-bid (below the 10.00 daypart floor)", w.Price)
	}
	// Inactive daypart: window not active → only the 0.50 base floor → the 3.00 bid wins.
	if w := h.ExtractWinner(t, h.RunAuction(t, inactiveID, "USA", "mobile", "daypart-inactive")); w.NoBid {
		t.Errorf("inactive-daypart auction no-bid, want a win (3.00 clears the 0.50 base floor)")
	}

	// Read-back: the list returns floor_config.dayparts so the portal can edit it.
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/placements", nil)
	resp, _ = pub.Do(req)
	var pls []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&pls)
	resp.Body.Close()
	for _, p := range pls {
		if p["ID"] == activeID {
			fc, _ := p["FloorConfig"].(map[string]any)
			dps, _ := fc["dayparts"].([]any)
			if len(dps) != 1 {
				t.Errorf("active placement dayparts = %v, want 1 entry", fc["dayparts"])
			}
		}
	}
}
