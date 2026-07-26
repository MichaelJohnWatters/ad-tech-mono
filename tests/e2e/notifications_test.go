//go:build e2e

// The in-app notifications service (cmd/notifications, :8096) consumes business
// events off NATS and writes one per-account row for the portal bell. It had no
// e2e at all. This proves the whole chain end to end: a campaign state change →
// SubjectCampaignStateChanged → the notifications consumer → the notifications
// table → the tenant-scoped portal API (list + unread count + mark-read).
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCampaignStateNotificationReachesPortal(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "notif")

	// A real logged-in session for the SAME account the campaign belongs to —
	// the portal feed is tenant-scoped to the caller's account.
	uniq := fmt.Sprintf("notif-%d", time.Now().UnixNano())
	email := uniq + "@api.test"
	h.CreateLoginUser(t, w.AdvAcc.ID, email, "pw-e2e-1", "owner")
	client := h.LoginAs(t, email, "pw-e2e-1")

	// Trigger a live → paused transition; the notifications service writes a
	// single campaign_state row keyed to this campaign.
	h.PatchCampaignStatus(t, w.Campaign, "paused")

	// Poll the bell API until it lands (NATS → consumer → Postgres is async).
	var notifID string
	harness.WaitFor(t, 20*time.Second, "campaign_state notification in the portal feed", func() bool {
		resp := h.APIJSON(t, client, http.MethodGet, "/v1/api/notifications", "")
		list, _ := resp["notifications"].([]any)
		for _, n := range list {
			m, _ := n.(map[string]any)
			if m["kind"] == "campaign_state" && m["ref_id"] == w.Campaign.ID {
				notifID, _ = m["id"].(string)
				return true
			}
		}
		return false
	})
	if notifID == "" {
		t.Fatal("no campaign_state notification surfaced for the paused campaign")
	}

	// The unread badge reflects it.
	resp := h.APIJSON(t, client, http.MethodGet, "/v1/api/notifications", "")
	if unread, _ := resp["unread"].(float64); unread < 1 {
		t.Errorf("unread = %v, want >= 1", resp["unread"])
	}

	// Mark-read clears it (and stays tenant-scoped — this is the caller's row).
	h.APIJSON(t, client, http.MethodPost, "/v1/api/notifications/read",
		fmt.Sprintf(`{"id":%q}`, notifID))
	resp = h.APIJSON(t, client, http.MethodGet, "/v1/api/notifications", "")
	list, _ := resp["notifications"].([]any)
	for _, n := range list {
		m, _ := n.(map[string]any)
		if m["id"] == notifID {
			if read, _ := m["read"].(bool); !read {
				t.Errorf("notification %s still unread after mark-read", notifID)
			}
		}
	}
}
