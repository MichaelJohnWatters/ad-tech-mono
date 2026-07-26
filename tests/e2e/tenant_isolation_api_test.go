//go:build e2e

// Cross-tenant isolation at the API/serving layer (NOT the DB-RLS layer that
// rls_test.go covers). Advertiser B must never be able to read or mutate
// advertiser A's campaigns through the gateway, even though B holds a perfectly
// valid session of its own. This locks in the CallerScope.CanMutate check in
// cmd/dsp/management.go — the fix that closed the campaign IDOR the API-key-only
// gate left open — so it can't silently regress. Nothing here asserted it before.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestTenantIsolationCampaignAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("iso-%d", time.Now().UnixNano())
	victim := h.Signup(t, "Iso Victim", uniq+"-a@api.test", "pw-e2e-1", "advertiser")
	attacker := h.Signup(t, "Iso Attacker", uniq+"-b@api.test", "pw-e2e-1", "advertiser")

	// Victim (account A) creates a campaign through the real portal API.
	created := h.APIJSON(t, victim, http.MethodPost, "/v1/api/campaigns", `{
		"name":"Victim Campaign",
		"base_bid":1.50,
		"daily_budget":100,
		"bid_strategy":"cpm"
	}`)
	victimID, _ := created["id"].(string)
	if victimID == "" {
		t.Fatalf("victim create returned no id: %v", created)
	}

	// The attacker (account B) has a valid session but does not own the campaign.
	t.Run("cross_tenant_patch_forbidden", func(t *testing.T) {
		got := h.APIStatus(t, attacker, http.MethodPatch, "/v1/api/campaigns/"+victimID,
			`{"base_bid":9999,"status":"paused"}`)
		if got != http.StatusForbidden && got != http.StatusNotFound {
			t.Errorf("attacker PATCH of victim campaign = %d, want 403 (or 404) — cross-tenant mutation must be blocked", got)
		}
	})

	t.Run("cross_tenant_delete_forbidden", func(t *testing.T) {
		got := h.APIStatus(t, attacker, http.MethodDelete, "/v1/api/campaigns/"+victimID, "")
		if got != http.StatusForbidden && got != http.StatusNotFound {
			t.Errorf("attacker DELETE of victim campaign = %d, want 403 (or 404)", got)
		}
	})

	t.Run("cross_tenant_list_excludes_victim", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/campaigns", nil)
		resp, err := attacker.Do(req)
		if err != nil {
			t.Fatalf("attacker list: %v", err)
		}
		var list []map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		for _, c := range list {
			if c["ID"] == victimID || c["id"] == victimID {
				t.Errorf("attacker's campaign list leaked victim campaign %s", victimID)
			}
		}
	})

	// Unauthenticated caller (no session cookie) must not reach the mutation
	// path at all.
	t.Run("unauthenticated_patch_rejected", func(t *testing.T) {
		got := h.APIStatus(t, http.DefaultClient, http.MethodPatch, "/v1/api/campaigns/"+victimID,
			`{"status":"paused"}`)
		if got != http.StatusUnauthorized && got != http.StatusForbidden {
			t.Errorf("unauthenticated PATCH = %d, want 401/403", got)
		}
	})

	// Sanity: the victim itself CAN still mutate its own campaign (proves the
	// 403s above are tenant scoping, not a blanket block on the endpoint).
	t.Run("owner_patch_allowed", func(t *testing.T) {
		got := h.APIStatus(t, victim, http.MethodPatch, "/v1/api/campaigns/"+victimID,
			`{"base_bid":2.00}`)
		if got != http.StatusNoContent && got != http.StatusOK {
			t.Errorf("owner PATCH of own campaign = %d, want 204/200", got)
		}
	})
}
