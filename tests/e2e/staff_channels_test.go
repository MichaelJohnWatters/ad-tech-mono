//go:build e2e

// Staff channel-activity oversight: platform staff see delivered impressions +
// spend broken down by channel across ALL advertisers via /v1/api/staff/channels
// (a platform-scoped reporting query — no account filter). Proves a DOOH play
// shows up under the 'dooh' channel in the staff breakdown.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestStaffChannelActivity(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "staff-chan")

	// Deliver one DOOH impression (channel=dooh) so the breakdown has a
	// distinctive channel to surface.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid to fire an impression")
	}
	h.FireDOOHPlay(t, auc.TraceID, win.CampaignID, win.CreativeID, auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, 1)

	// Staff session (staff account + owner role → support:read).
	staff := h.CreateStaff(t, fmt.Sprintf("staff-chan-acct-%d", time.Now().UnixNano()))
	staffEmail := fmt.Sprintf("staff-chan-%d@e2e.local", time.Now().UnixNano())
	h.CreateLoginUser(t, staff.ID, staffEmail, "e2e-pass", "owner")
	client := h.LoginAs(t, staffEmail, "e2e-pass")

	doohCount := func() int {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.APIStaffChannels, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("staff channels call: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("staff channels status %d: %s", resp.StatusCode, b)
		}
		var res struct {
			Columns []string        `json:"columns"`
			Rows    [][]interface{} `json:"rows"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ci, ni := indexOf(res.Columns, "channel"), indexOf(res.Columns, "count")
		if ci < 0 || ni < 0 {
			t.Fatalf("channel/count columns missing: %v", res.Columns)
		}
		for _, r := range res.Rows {
			if fmt.Sprint(r[ci]) == "dooh" {
				n, _ := r[ni].(float64)
				return int(n)
			}
		}
		return 0
	}

	deadline := time.Now().Add(30 * time.Second)
	for doohCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("dooh channel never appeared in the staff channel-activity breakdown")
		}
		time.Sleep(2 * time.Second)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
