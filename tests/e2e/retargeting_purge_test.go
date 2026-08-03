//go:build e2e

// Physical purge of expired retargeting members: read paths already exclude
// expired rows (correctness), but cmd/audience-rt also periodically DELETES them
// so dead rows don't accumulate. This proves an expired member is physically gone
// after the purge interval, while a live member is untouched.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetargetingMemberPurge(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rt-purge")
	uniq := time.Now().UnixNano()

	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'portal', 'dsp_private',
        jsonb_build_object('event','site_visit','tag','purge','min_count',1,'window_days',30))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("rt-purge-%d", uniq)).Scan(&segID); err != nil {
		t.Fatalf("seed segment: %v", err)
	}
	expired := fmt.Sprintf("purge-expired-%d", uniq)
	live := fmt.Sprintf("purge-live-%d", uniq)
	// One already-expired member, one live (expires in the future).
	if _, err := h.DB.Exec(`
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at, expires_at) VALUES
  ($1, $2, $3::uuid, now(), now() - interval '1 minute'),
  ($1, $4, $3::uuid, now(), now() + interval '30 days')`, segID, expired, w.AdvAcc.ID, live); err != nil {
		t.Fatalf("seed members: %v", err)
	}

	count := func(uid string) int {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id=$1 AND user_id=$2`, segID, uid).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// The purge ticker (default 60s) physically removes the expired row.
	deadline := time.Now().Add(100 * time.Second)
	for count(expired) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("expired member was never physically purged")
		}
		time.Sleep(3 * time.Second)
	}
	// The live member must survive the purge.
	if count(live) != 1 {
		t.Error("purge deleted a LIVE (non-expired) member")
	}
}
