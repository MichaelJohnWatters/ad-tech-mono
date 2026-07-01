//go:build e2e

// Audience write-path test — the CRM upload endpoint (create segment +
// bulk-add members), the production counterpart to the direct-SQL harness
// helpers.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAudienceUploadWritesMembers(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "aud-upload")

	users := []string{"crm-u1", "crm-u2", "crm-u3"}
	segID := h.UploadAudience(t, w.AdvAcc.ID, "e2e-crm-list", "dsp_private", users)
	if segID == "" {
		t.Fatal("upload returned empty segment id")
	}
	if got := h.SegmentMemberCount(t, segID); got != len(users) {
		t.Fatalf("segment has %d members, want %d", got, len(users))
	}

	// Idempotent: re-uploading the same list to the same named segment adds
	// no duplicates (same deterministic segment id, ON CONFLICT DO NOTHING).
	segID2 := h.UploadAudience(t, w.AdvAcc.ID, "e2e-crm-list", "dsp_private", append(users, "crm-u4"))
	if segID2 != segID {
		t.Errorf("re-upload created a new segment %s, want stable %s", segID2, segID)
	}
	if got := h.SegmentMemberCount(t, segID); got != 4 {
		t.Errorf("after re-upload with one new user, members = %d, want 4", got)
	}
}
