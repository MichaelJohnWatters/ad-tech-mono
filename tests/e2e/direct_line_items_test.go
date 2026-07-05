//go:build e2e

// Publisher direct-sold line-item CRUD via the real API. The publisher-adserver
// already served these from a warm cache; this is the publisher-facing write
// path (create/list/patch), tenant-scoped, publishing the publisher-line-items
// cache invalidate.
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

func TestDirectLineItemsViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("pli-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Direct Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Direct Site","domain":"`+uniq+`.test"}`)
	siteID := site["id"].(string)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Direct MPU","format":"display","width":300,"height":250,"floor_price":0.50
	}`, siteID))
	placementID := pl["id"].(string)

	// Create a sponsorship line item scoped to the owned placement.
	created := h.APIJSON(t, pub, http.MethodPost, "/v1/api/direct-line-items", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Nike Homepage Sponsorship","demand_source":"Nike",
		"priority_tier":"sponsorship","cpm":50.0,"impressions_committed":1000000,
		"pacing_mode":"even","placement_ids":[%q]
	}`, siteID, placementID))
	id := created["id"].(string)

	// List it back and check the round-trip.
	list := listDirect(t, h, pub)
	var found map[string]any
	for _, it := range list {
		if it["id"] == id {
			found = it
		}
	}
	if found == nil {
		t.Fatalf("created line item %s not in list", id)
	}
	if found["priority_tier"] != "sponsorship" {
		t.Errorf("priority_tier = %v, want sponsorship", found["priority_tier"])
	}
	if found["cpm"] != 50.0 {
		t.Errorf("cpm = %v, want 50", found["cpm"])
	}
	if found["status"] != "active" {
		t.Errorf("status = %v, want active", found["status"])
	}

	// PATCH: pause + retier to house.
	h.APIJSON(t, pub, http.MethodPatch, "/v1/api/direct-line-items/"+id, `{"status":"paused","priority_tier":"house"}`)
	list = listDirect(t, h, pub)
	for _, it := range list {
		if it["id"] == id {
			if it["status"] != "paused" {
				t.Errorf("after patch status = %v, want paused", it["status"])
			}
			if it["priority_tier"] != "house" {
				t.Errorf("after patch tier = %v, want house", it["priority_tier"])
			}
		}
	}

	// A placement allowlist referencing another account's placement is rejected.
	otherPub := h.Signup(t, "Other Pub", uniq+"-other@api.test", "pw-e2e-1", "publisher")
	otherSite := h.APIJSON(t, otherPub, http.MethodPost, "/v1/api/publishers", `{"name":"Other Site","domain":"`+uniq+`-o.test"}`)
	otherPl := h.APIJSON(t, otherPub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Other MPU","format":"display","width":300,"height":250,"floor_price":0.50
	}`, otherSite["id"]))
	req, _ := http.NewRequest(http.MethodPost, h.URLs.Gateway+"/v1/api/direct-line-items",
		strings.NewReader(fmt.Sprintf(`{"publisher_id":%q,"name":"x","priority_tier":"house","placement_ids":[%q]}`, siteID, otherPl["id"])))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := pub.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-account placement create = %d, want 403", resp.StatusCode)
	}

	// Tenant isolation: the other publisher can't see the first publisher's line item.
	for _, it := range listDirect(t, h, otherPub) {
		if it["id"] == id {
			t.Errorf("other publisher can see line item %s (tenant leak)", id)
		}
	}

	// An advertiser session is forbidden from the publisher endpoint.
	adv := h.Signup(t, "Direct Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/direct-line-items", nil)
	resp, _ = adv.Do(req)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusForbidden {
		t.Errorf("advertiser GET = %d, want 403", code)
	}
}

func listDirect(t *testing.T, h *harness.Harness, client *http.Client) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/direct-line-items", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("list direct: %v", err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}
