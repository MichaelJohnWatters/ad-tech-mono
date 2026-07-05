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

	// Staff session (the seeded admin has "*", covers support:read/update).
	staff := h.LoginAs(t, harness.DevAdminEmail, harness.DevPassword)

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

	// A publisher session is forbidden from the staff revshare editor.
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/revshare", nil)
	resp, _ = pub.Do(req)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusForbidden {
		t.Errorf("publisher GET revshare = %d, want 403", code)
	}
}
