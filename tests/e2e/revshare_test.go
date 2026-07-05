//go:build e2e

// Staff revenue-share editor: publishers.revshare_config was seed-only, but
// reporting's ContractLoader consumes it for the publisher/platform split.
// This is the missing staff write path — GET lists every publisher's fee,
// PATCH updates one, invalidating the billing-rates cache.
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

func TestRevshareEditingViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	// A publisher to edit (created via the real API by a publisher session).
	uniq := fmt.Sprintf("rev-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Rev Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Rev Site","domain":"`+uniq+`.test"}`)
	publisherID := site["id"].(string)

	// Staff session — Reset wiped the seeded admin, so mint an admin account
	// + login here (admin has "*", covers support:read/update).
	admin := h.CreateAdmin(t, uniq+"-admin")
	adminEmail := uniq + "-admin@login.test"
	h.CreateLoginUser(t, admin.ID, adminEmail, "pw-e2e-1", "owner")
	staff := h.LoginAs(t, adminEmail, "pw-e2e-1")

	// GET the revshare list — the new publisher defaults to 20%.
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/revshare", nil)
	resp, err := staff.Do(req)
	if err != nil {
		t.Fatalf("revshare list: %v", err)
	}
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	var found bool
	for _, p := range list {
		if p["publisher_id"] == publisherID {
			found = true
			if p["fee_pct"].(float64) != 20 {
				t.Errorf("default fee_pct = %v, want 20", p["fee_pct"])
			}
		}
	}
	if !found {
		t.Fatalf("new publisher %s not in revshare list", publisherID)
	}

	// PATCH the fee to 35% (staff).
	req, _ = http.NewRequest(http.MethodPatch, h.URLs.Gateway+"/v1/api/revshare?id="+publisherID,
		strings.NewReader(`{"revshare_model":"fixed","fee_pct":35}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = staff.Do(req)
	if err != nil {
		t.Fatalf("revshare patch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revshare patch = %d, want 200", resp.StatusCode)
	}

	// Read it back — the new fee persisted.
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/revshare", nil)
	resp, _ = staff.Do(req)
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	for _, p := range list {
		if p["publisher_id"] == publisherID && p["fee_pct"].(float64) != 35 {
			t.Errorf("after patch: fee_pct = %v, want 35", p["fee_pct"])
		}
	}

	// PATCH a full tiered config + payment terms, then read it back.
	req, _ = http.NewRequest(http.MethodPatch, h.URLs.Gateway+"/v1/api/revshare?id="+publisherID,
		strings.NewReader(`{"revshare_model":"tiered","payment_terms":"net_60",
			"tiers":[{"min_impressions":0,"max_impressions":1000000,"fee_pct":25},
			         {"min_impressions":1000000,"max_impressions":0,"fee_pct":18}],
			"guaranteed_min_cpm":0.5}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = staff.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tiered patch = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/revshare", nil)
	resp, _ = staff.Do(req)
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	for _, p := range list {
		if p["publisher_id"] != publisherID {
			continue
		}
		if p["revshare_model"] != "tiered" || p["payment_terms"] != "net_60" {
			t.Errorf("after tiered patch: model=%v terms=%v", p["revshare_model"], p["payment_terms"])
		}
		tiers, _ := p["tiers"].([]any)
		if len(tiers) != 2 {
			t.Fatalf("tiers round-trip = %v, want 2 entries", p["tiers"])
		}
		t0, _ := tiers[0].(map[string]any)
		if t0["fee_pct"].(float64) != 25 || t0["max_impressions"].(float64) != 1000000 {
			t.Errorf("tier[0] = %v, want fee 25 / max 1000000", t0)
		}
		if p["guaranteed_min_cpm"].(float64) != 0.5 {
			t.Errorf("guaranteed_min_cpm = %v, want 0.5", p["guaranteed_min_cpm"])
		}
	}

	// Non-contiguous tiers are rejected (money-touching validation).
	req, _ = http.NewRequest(http.MethodPatch, h.URLs.Gateway+"/v1/api/revshare?id="+publisherID,
		strings.NewReader(`{"revshare_model":"tiered","tiers":[{"min_impressions":0,"max_impressions":1000,"fee_pct":25},{"min_impressions":5000,"max_impressions":0,"fee_pct":18}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = staff.Do(req)
	badCode := resp.StatusCode
	resp.Body.Close()
	if badCode != http.StatusBadRequest {
		t.Errorf("non-contiguous tiers = %d, want 400", badCode)
	}

	// A publisher session is forbidden from the staff revshare editor.
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/revshare", nil)
	resp, _ = pub.Do(req)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusForbidden {
		t.Errorf("publisher GET revshare = %d, want 403", code)
	}
}
