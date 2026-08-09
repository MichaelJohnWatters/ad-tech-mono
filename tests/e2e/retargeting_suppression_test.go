//go:build e2e

// Durable purchase suppression (migration 082, the "burn list"). Before it,
// suppression only DELETED the buyer's membership rows — the hourly
// profile-builder re-qualified them from still-live site_visit signals on
// its next pass and the chase resumed within the hour (found live
// 2026-08-09). This proves the closed loop:
//
//	visit → enrolled (audience-rt) → purchase → suppressed + burn-listed →
//	the builder runs and does NOT re-enroll the buyer (while a CONTROL user
//	with identical signals IS re-enrolled — so the signals were there and
//	the builder genuinely filtered) → a NEW visit clears the burn list and
//	the chase legitimately restarts.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestPurchaseSuppressionDurableAcrossBuilderRuns(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rt-burnlist")
	uniq := time.Now().UnixNano()
	buyer := fmt.Sprintf("burn-buyer-%d", uniq)
	control := fmt.Sprintf("burn-control-%d", uniq)
	tag := fmt.Sprintf("cart-burn-%d", uniq)

	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'profile_builder', 'public',
        jsonb_build_object('event', 'site_visit', 'tag', $3::text, 'min_count', 1))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("burn-seg-%d", uniq), tag).Scan(&segID); err != nil {
		t.Fatalf("create retargeting segment: %v", err)
	}
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})

	member := func(uid string) bool {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`,
			segID, uid).Scan(&n); err != nil {
			t.Fatalf("membership query: %v", err)
		}
		return n > 0
	}
	waitMember := func(uid string, want bool, what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for member(uid) != want {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s membership=%v (%s)", uid, want, what)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	suppressed := func(uid string) bool {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM retargeting_suppressions WHERE account_id = $1::uuid AND user_id = $2 AND expires_at > now()`,
			w.AdvAcc.ID, uid).Scan(&n); err != nil {
			t.Fatalf("suppression query: %v", err)
		}
		return n > 0
	}

	// Both users abandon a cart; audience-rt enrolls both in seconds.
	fireVisit(t, w.AdvAcc.ID, tag, buyer)
	fireVisit(t, w.AdvAcc.ID, tag, control)
	waitMember(buyer, true, "buyer enrolled on visit")
	waitMember(control, true, "control enrolled on visit")

	// The buyer purchases: membership removed AND a durable burn-list entry
	// written (the new half).
	h.FireConversionForVisitor(t, fmt.Sprintf("burn-conv-%d", uniq), w.AdvAcc.ID, buyer, "purchase", "USD", 42.00)
	waitMember(buyer, false, "buyer suppressed on purchase")
	if !suppressed(buyer) {
		t.Fatal("purchase did not write a burn-list entry (retargeting_suppressions)")
	}

	// Remove the control's row directly, then run the builder until it
	// re-enrolls the control from the ClickHouse signals. That PROVES the
	// signals are visible and the rule pass ran — so the buyer staying out
	// is the suppression filter working, not missing data. (CH ingest is
	// async; retry the run briefly until the control reappears.)
	if _, err := h.DB.Exec(`DELETE FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`, segID, control); err != nil {
		t.Fatalf("remove control member: %v", err)
	}
	deadline := time.Now().Add(45 * time.Second)
	for !member(control) {
		if time.Now().After(deadline) {
			t.Fatal("builder never re-enrolled the CONTROL user — signals absent or rule pass broken; suppression untestable")
		}
		runBuilder(t, h, lakeStore(t, h))
		time.Sleep(2 * time.Second)
	}
	if member(buyer) {
		t.Fatal("builder RE-ENROLLED the buyer from pre-purchase signals — the burn list was not consulted")
	}

	// A genuinely NEW abandoned cart reopens the chase: the visit clears the
	// burn-list entry and enrolls immediately.
	fireVisit(t, w.AdvAcc.ID, tag, buyer)
	waitMember(buyer, true, "new visit re-enrolls after purchase")
	deadline = time.Now().Add(10 * time.Second)
	for suppressed(buyer) {
		if time.Now().After(deadline) {
			t.Fatal("new visit did not clear the burn-list entry")
		}
		time.Sleep(500 * time.Millisecond)
	}
}
