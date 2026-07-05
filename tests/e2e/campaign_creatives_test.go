//go:build e2e

// Attaching uploaded creatives to a campaign with rotation weights. Creative
// upload + the library already existed; this proves the new campaign PATCH
// `creatives` field replaces line_item_creatives (approved + owned only) and
// that the attachment flows to bidding — the DSP picks the attached creative.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCampaignCreativeAttachViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("crat-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Crat Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Crat Site","domain":"`+uniq+`.test"}`)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Crat MPU","format":"display","width":300,"height":250,"floor_price":0.50
	}`, site["id"]))
	placementID := pl["id"].(string)

	adv := h.Signup(t, "Crat Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{"name":"Crat","base_bid":3.0,"daily_budget":500}`)
	campaignID := created["id"].(string)
	advAccountID := created["account_id"].(string)
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")

	// Upload a 300×250 creative → pending_review.
	up := h.APIJSON(t, adv, http.MethodPost, "/v1/api/creatives", `{
		"name":"AttachMe","format":"display","width":300,"height":250,
		"html_content":"<div>attach me</div>","landing_url":"https://attach.test"
	}`)
	attachID := up["id"].(string)

	// A pending (unapproved) creative can't be attached yet → 400.
	if code := h.APIStatus(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID,
		fmt.Sprintf(`{"creatives":[{"creative_id":%q,"weight":1}]}`, attachID)); code != http.StatusBadRequest {
		t.Fatalf("attach unapproved creative = %d, want 400", code)
	}

	// Approve it via the staff moderation queue (mint an admin — Reset wiped the seed).
	admin := h.CreateAdmin(t, uniq+"-admin")
	adminEmail := uniq + "-admin@login.test"
	h.CreateLoginUser(t, admin.ID, adminEmail, "pw-e2e-1", "owner")
	staff := h.LoginAs(t, adminEmail, "pw-e2e-1")
	if code := h.APIStatus(t, staff, http.MethodPost, "/v1/api/moderation",
		fmt.Sprintf(`{"creative_id":%q,"action":"approve"}`, attachID)); code != http.StatusOK {
		t.Fatalf("approve creative = %d, want 200", code)
	}

	// Now attach the approved creative (replaces the auto-generated one).
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID,
		fmt.Sprintf(`{"creative_rotation":"weighted","creatives":[{"creative_id":%q,"weight":5}]}`, attachID))
	h.RefreshAllCaches(t)

	// The auction now serves the attached creative.
	w := h.ExtractWinner(t, h.RunAuction(t, placementID, "USA", "mobile", uniq+"-u"))
	if w.NoBid {
		t.Fatalf("auction no-bid, want a win with the attached creative")
	}
	if w.CreativeID != attachID {
		t.Errorf("winning creative = %q, want the attached %q", w.CreativeID, attachID)
	}

	// A random (non-owned) creative id is rejected.
	if code := h.APIStatus(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID,
		`{"creatives":[{"creative_id":"11111111-1111-4111-8111-111111111111","weight":1}]}`); code != http.StatusBadRequest {
		t.Errorf("attach non-owned creative = %d, want 400", code)
	}
}
