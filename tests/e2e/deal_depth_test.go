//go:build e2e

// Deal depth via the real API: create/edit now expose advertiser/placement
// allowlists, flight dates and PG guaranteed volume (previously the API
// wrote none of them, so every deal was effectively "open to any advertiser
// on any placement"). The exchange matcher already consumes these fields —
// this proves the missing write path persists them and enforces placement
// ownership. Matcher *behaviour* is covered by deals_matrix_test.go.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDealDepthViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("dealdepth-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Deal Pub", uniq+"@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers",
		`{"name":"Deal Site","domain":"`+uniq+`.test"}`)
	siteID := site["id"].(string)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements",
		fmt.Sprintf(`{"publisher_id":%q,"name":"Deal MPU","format":"display","width":300,"height":250,"floor_price":0.5}`, siteID))
	placementID := pl["id"].(string)

	// A fixed advertiser UUID for the allowlist (external — the publisher
	// grants it access; no ownership check on advertiser IDs).
	advUUID := "dddddddd-1111-4111-8111-111111111111"

	// Create a PMP deal with both allowlists, a flight window and (ignored
	// for PMP but accepted) fields — through the real API.
	created := h.APIJSON(t, pub, http.MethodPost, "/v1/api/deals", fmt.Sprintf(`{
		"publisher_id":%q, "name":"Depth PMP", "deal_type":"pmp", "price":3.25,
		"advertiser_ids":[%q], "placement_ids":[%q],
		"start_date":"2026-07-01", "end_date":"2026-12-31"
	}`, siteID, advUUID, placementID))
	dealID := created["id"].(string)
	if dealID == "" {
		t.Fatalf("deal create returned no id: %v", created)
	}

	// Read it back — the list now returns the allowlists + dates.
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/deals", nil)
	resp, err := pub.Do(req)
	if err != nil {
		t.Fatalf("deal list: %v", err)
	}
	var deals []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&deals)
	resp.Body.Close()
	var got map[string]any
	for _, d := range deals {
		if d["id"] == dealID {
			got = d
		}
	}
	if got == nil {
		t.Fatalf("created deal %s not in list %v", dealID, deals)
	}
	advList, _ := got["advertiser_ids"].([]any)
	plList, _ := got["placement_ids"].([]any)
	if len(advList) != 1 || advList[0] != advUUID {
		t.Errorf("advertiser_ids = %v, want [%s]", got["advertiser_ids"], advUUID)
	}
	if len(plList) != 1 || plList[0] != placementID {
		t.Errorf("placement_ids = %v, want [%s]", got["placement_ids"], placementID)
	}
	if got["start_date"] != "2026-07-01" || got["end_date"] != "2026-12-31" {
		t.Errorf("flight window = %v..%v, want 2026-07-01..2026-12-31", got["start_date"], got["end_date"])
	}

	// PATCH clears the advertiser allowlist (empty = match-all) and updates
	// the placement allowlist — the depth edit path.
	h.APIJSON(t, pub, http.MethodPatch, "/v1/api/deals/"+dealID,
		fmt.Sprintf(`{"advertiser_ids":[],"placement_ids":[%q]}`, placementID))
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/deals", nil)
	resp, _ = pub.Do(req)
	_ = json.NewDecoder(resp.Body).Decode(&deals)
	resp.Body.Close()
	for _, d := range deals {
		if d["id"] == dealID {
			if adv, _ := d["advertiser_ids"].([]any); len(adv) != 0 {
				t.Errorf("after clear: advertiser_ids = %v, want empty", d["advertiser_ids"])
			}
		}
	}

	// Placement-ownership guard: a deal referencing a placement the account
	// doesn't own is 403, not a silent accept.
	req, _ = http.NewRequest(http.MethodPost, h.URLs.Gateway+"/v1/api/deals",
		strings.NewReader(fmt.Sprintf(`{"publisher_id":%q,"name":"Foreign","deal_type":"pmp","placement_ids":["eeeeeeee-1111-4111-8111-111111111111"]}`, siteID)))
	req.Header.Set("Content-Type", "application/json")
	resp, err = pub.Do(req)
	if err != nil {
		t.Fatalf("foreign placement create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign placement allowlist = %d, want 403", resp.StatusCode)
	}

	// Bad UUID in an allowlist → 400.
	req, _ = http.NewRequest(http.MethodPost, h.URLs.Gateway+"/v1/api/deals",
		strings.NewReader(fmt.Sprintf(`{"publisher_id":%q,"name":"Bad","deal_type":"pmp","advertiser_ids":["not-a-uuid"]}`, siteID)))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = pub.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad advertiser UUID = %d, want 400", resp.StatusCode)
	}
}
