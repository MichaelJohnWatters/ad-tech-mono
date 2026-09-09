//go:build e2e

// Multi-replica exactly-once: audience-rt runs 2 replicas, so a burst of visits
// must NOT double-count. Firing K distinct shoppers, each TWICE, concurrently
// across both pods must yield EXACTLY K enrollments (idempotent upsert — no double
// mutation) and EXACTLY K retargeting.enrolled webhooks (queue-group + first-enroll
// — no double action), never 2K and never > K.
package e2e

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetargetingMultiReplicaExactlyOnce(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rt-multireplica")
	uniq := time.Now().UnixNano()
	tag := fmt.Sprintf("mr-%d", uniq)

	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'portal', 'dsp_private',
        jsonb_build_object('event','site_visit','tag',$3::text,'min_count',1,'window_days',30))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("rt-mr-%d", uniq), tag).Scan(&segID); err != nil {
		t.Fatalf("seed segment: %v", err)
	}
	var hookID string
	if err := h.DB.QueryRow(`
INSERT INTO webhooks (account_id, url, events, secret, status)
VALUES ($1::uuid, 'http://127.0.0.1:1/hook', ARRAY['retargeting.enrolled'], 'e2e-secret', 'active')
RETURNING id::text`, w.AdvAcc.ID).Scan(&hookID); err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	const K = 10
	// K shoppers, each visits TWICE, all fired concurrently so both audience-rt
	// pods are active at once (errors ignored here — the count assertions below
	// catch any dropped or doubled work).
	var wg sync.WaitGroup
	for i := 0; i < K; i++ {
		uid := fmt.Sprintf("mr-visitor-%d-%d", uniq, i)
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func(u string) {
				defer wg.Done()
				url := fmt.Sprintf("http://localhost:8083/v1/t/rt?aid=%s&tag=%s&uid=%s", w.AdvAcc.ID, tag, u)
				if resp, err := http.Get(url); err == nil {
					resp.Body.Close()
				}
			}(uid)
		}
	}
	wg.Wait()

	// Count SHOPPER and HOUSEHOLD members separately. With audience_rt.household_enroll
	// on (the deployed default), each visit ALSO enrolls the visit's household
	// (hh:<salted-IP hash>). Every fire here comes from the SAME loopback IP, so
	// that's ONE shared household — and it must enroll exactly ONCE despite 20
	// concurrent fires across both pods (a stronger exactly-once proof than the
	// shoppers, deduped by the (segment_id,user_id) PK). Asserting the two
	// populations separately keeps the test correct whether or not the gate is on.
	shopperMembers := func() int {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id=$1 AND user_id NOT LIKE 'hh:%'`, segID).Scan(&n); err != nil {
			t.Fatalf("shopper member count: %v", err)
		}
		return n
	}
	householdMembers := func() int {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id=$1 AND user_id LIKE 'hh:%'`, segID).Scan(&n); err != nil {
			t.Fatalf("household member count: %v", err)
		}
		return n
	}
	// Exactly K shopper members — one per shopper, no doubles from the two visits
	// or the two pods.
	deadline := time.Now().Add(30 * time.Second)
	for shopperMembers() < K {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d shoppers enrolled (dropped work under replicas?)", shopperMembers(), K)
		}
		time.Sleep(time.Second)
	}
	time.Sleep(4 * time.Second) // let any straggler double-write surface
	if n := shopperMembers(); n != K {
		t.Errorf("enrolled %d shopper members, want EXACTLY %d (double-enroll under replicas)", n, K)
	}
	// The one shared household (same loopback IP for all fires) must enroll EXACTLY
	// once: PRESENT (household enrollment working — audience_rt.household_enroll
	// defaults on, and it rides the same OnSiteVisit as the shoppers) AND deduped
	// (never 2+ despite 20 concurrent fires across both pods). Assert both bounds:
	// a bare `<=1` would let a silently-broken enroll path (gate wrongly off, event
	// dropped) pass as 0. Poll for it to appear (same async path as shoppers), then
	// assert no double.
	harness.WaitFor(t, 15*time.Second, "shared household enrolled", func() bool {
		return householdMembers() >= 1
	})
	if n := householdMembers(); n != 1 {
		t.Errorf("shared household enrolled %d times, want EXACTLY 1 (enrolled once + deduped under replicas)", n)
	}

	// Exactly K webhook dispatches (attempt=1 = one per dispatched event; retries
	// add attempts 2..3 to the same event).
	dispatches := func() int {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE webhook_id=$1 AND event_type='retargeting.enrolled' AND attempt=1`, hookID).Scan(&n); err != nil {
			t.Fatalf("dispatch count: %v", err)
		}
		return n
	}
	dl2 := time.Now().Add(20 * time.Second)
	for dispatches() < K {
		if time.Now().After(dl2) {
			t.Fatalf("only %d of %d enrolled webhooks dispatched", dispatches(), K)
		}
		time.Sleep(time.Second)
	}
	time.Sleep(4 * time.Second)
	if n := dispatches(); n != K {
		t.Errorf("dispatched %d retargeting.enrolled webhooks, want EXACTLY %d (double-fire under replicas)", n, K)
	}
}
