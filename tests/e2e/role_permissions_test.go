//go:build e2e

// Role-based permission scoping: a read-only "viewer" (advertiser:viewer =
// campaigns:read/creatives:read/billing:view/reports:read — no write perms) may
// LIST campaigns but must be blocked from mutating one, even within its own
// account. The gateway gates PATCH on campaigns:update
// (RequirePermissionByMethod, cmd/gateway/main.go). Nothing asserted the
// read/write role split end to end.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestViewerRoleCannotMutateCampaign(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("role-%d", time.Now().UnixNano())
	ownerEmail := uniq + "-owner@api.test"
	owner := h.Signup(t, "Role Owner", ownerEmail, "pw-e2e-1", "advertiser")
	accID := accountIDByEmail(t, h, ownerEmail)

	// Owner creates a campaign.
	created := h.APIJSON(t, owner, http.MethodPost, "/v1/api/campaigns", `{
		"name":"Role Campaign","base_bid":1.50,"daily_budget":100,"bid_strategy":"cpm"
	}`)
	campaignID, _ := created["id"].(string)
	if campaignID == "" {
		t.Fatalf("create returned no id: %v", created)
	}

	// A viewer on the SAME account.
	viewerEmail := uniq + "-viewer@api.test"
	h.CreateLoginUser(t, accID, viewerEmail, "pw-e2e-1", "viewer")
	viewer := h.LoginAs(t, viewerEmail, "pw-e2e-1")

	t.Run("viewer_can_read", func(t *testing.T) {
		if got := h.APIStatus(t, viewer, http.MethodGet, "/v1/api/campaigns", ""); got != http.StatusOK {
			t.Errorf("viewer GET campaigns = %d, want 200 (viewer has campaigns:read)", got)
		}
	})

	t.Run("viewer_cannot_patch", func(t *testing.T) {
		if got := h.APIStatus(t, viewer, http.MethodPatch, "/v1/api/campaigns/"+campaignID,
			`{"base_bid":9.99}`); got != http.StatusForbidden {
			t.Errorf("viewer PATCH campaign = %d, want 403 (no campaigns:update)", got)
		}
	})

	t.Run("viewer_cannot_delete", func(t *testing.T) {
		if got := h.APIStatus(t, viewer, http.MethodDelete, "/v1/api/campaigns/"+campaignID, ""); got != http.StatusForbidden {
			t.Errorf("viewer DELETE campaign = %d, want 403 (no campaigns:delete)", got)
		}
	})

	// Control: the owner CAN mutate — proves the 403s are the role gate, not a
	// broken endpoint.
	t.Run("owner_can_patch", func(t *testing.T) {
		got := h.APIStatus(t, owner, http.MethodPatch, "/v1/api/campaigns/"+campaignID, `{"base_bid":2.00}`)
		if got != http.StatusNoContent && got != http.StatusOK {
			t.Errorf("owner PATCH = %d, want 204/200", got)
		}
	})
}
