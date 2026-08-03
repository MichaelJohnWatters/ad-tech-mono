//go:build e2e

// Staff retargeting oversight: platform staff see every advertiser's retargeting
// audiences (tag, window, live enrollment) across tenants via /v1/api/staff/
// retargeting. Proves the staff-only, cross-tenant read returns a real audience
// with its enrollment count.
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

func TestStaffRetargetingOversight(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "staff-rt")
	uniq := time.Now().UnixNano()
	segName := fmt.Sprintf("staff-rt-seg-%d", uniq)
	tag := fmt.Sprintf("staff-tag-%d", uniq)

	// An advertiser's retargeting audience with one enrolled member.
	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'portal', 'dsp_private',
        jsonb_build_object('event','site_visit','tag',$3::text,'min_count',1,'window_days',21))
RETURNING id::text`, w.AdvAcc.ID, segName, tag).Scan(&segID); err != nil {
		t.Fatalf("seed retargeting segment: %v", err)
	}
	if _, err := h.DB.Exec(`
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at)
VALUES ($1, $2, $3::uuid, now())`, segID, fmt.Sprintf("staff-vis-%d", uniq), w.AdvAcc.ID); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// Authenticate as platform staff (staff account + owner role → staff:owner
	// perms, which include support:read).
	staff := h.CreateStaff(t, fmt.Sprintf("staff-rt-acct-%d", uniq))
	staffEmail := fmt.Sprintf("staff-rt-%d@e2e.local", uniq)
	h.CreateLoginUser(t, staff.ID, staffEmail, "e2e-pass", "owner")
	client := h.LoginAs(t, staffEmail, "e2e-pass")

	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.APIStaffRetargeting, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("staff retargeting call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("staff retargeting status %d: %s", resp.StatusCode, b)
	}
	var rows []struct {
		Advertiser string `json:"advertiser"`
		Segment    string `json:"segment"`
		Tag        string `json:"tag"`
		WindowDays int    `json:"window_days"`
		Members    int    `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Segment == segName {
			found = true
			if r.Tag != tag || r.WindowDays != 21 {
				t.Errorf("row = %+v, want tag %s window 21", r, tag)
			}
			if r.Members < 1 {
				t.Errorf("enrollment = %d, want >= 1", r.Members)
			}
			if r.Advertiser == "" {
				t.Error("advertiser name missing from staff oversight row")
			}
		}
	}
	if !found {
		t.Fatalf("seeded retargeting audience %q not visible in staff oversight", segName)
	}
}
