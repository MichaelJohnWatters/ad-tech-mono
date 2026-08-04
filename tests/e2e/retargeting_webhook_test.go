//go:build e2e

// Abandon→push trigger: when a shopper is enrolled into a retargeting audience,
// audience-rt emits adtech.retargeting.enrolled (account-scoped), and the webhooks
// dispatcher delivers it to the advertiser's registered endpoints — the hook an
// advertiser uses to fire an abandoned-cart email/re-engagement in real time.
// Proves the event flows to a webhook delivery attempt (the receiver URL is
// unreachable on purpose — every attempt is logged regardless of outcome).
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetargetingEnrolledWebhook(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rt-hook")
	uniq := time.Now().UnixNano()
	tag := fmt.Sprintf("hook-%d", uniq)

	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'portal', 'dsp_private',
        jsonb_build_object('event','site_visit','tag',$3::text,'min_count',1,'window_days',30))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("rt-hook-%d", uniq), tag).Scan(&segID); err != nil {
		t.Fatalf("seed segment: %v", err)
	}
	// A webhook subscribed to retargeting.enrolled. The URL is intentionally
	// unreachable — we assert only that a delivery ATTEMPT is logged.
	var hookID string
	if err := h.DB.QueryRow(`
INSERT INTO webhooks (account_id, url, events, secret, status)
VALUES ($1::uuid, 'http://127.0.0.1:1/hook', ARRAY['retargeting.enrolled'], 'e2e-secret', 'active')
RETURNING id::text`, w.AdvAcc.ID).Scan(&hookID); err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	visitor := fmt.Sprintf("hook-visitor-%d", uniq)

	// One dispatched event = one attempt=1 delivery row (retries add attempts
	// 2..3 to the same event, so attempt=1 counts distinct dispatches).
	dispatches := func() int {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE webhook_id=$1 AND event_type='retargeting.enrolled' AND attempt=1`, hookID).Scan(&n); err != nil {
			t.Fatalf("delivery query: %v", err)
		}
		return n
	}

	// A shopper visits → enrolled → audience-rt emits retargeting.enrolled →
	// webhooks delivers it.
	fireVisit(t, w.AdvAcc.ID, tag, visitor)
	deadline := time.Now().Add(30 * time.Second)
	for dispatches() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("retargeting.enrolled webhook was never delivered")
		}
		time.Sleep(2 * time.Second)
	}

	// trace_id flows all the way downstream: the pixel had no ?tid=, so the
	// behaviour event took the tracker's OTel trace, which rode through audience-rt
	// into the retargeting.enrolled event → the delivered webhook payload's data.
	var traceID string
	if err := h.DB.QueryRow(`SELECT COALESCE(payload->'data'->>'trace_id','') FROM webhook_deliveries WHERE webhook_id=$1 AND event_type='retargeting.enrolled' AND attempt=1 LIMIT 1`, hookID).Scan(&traceID); err != nil {
		t.Fatalf("payload trace_id query: %v", err)
	}
	if len(traceID) != 32 {
		t.Errorf("webhook payload trace_id = %q (len %d), want a 32-hex OTel trace flowed from the visit", traceID, len(traceID))
	}

	// First-enroll-only: the SAME shopper visiting again is already a member, so
	// no second event fires (silent window refresh) — the webhook does NOT re-fire.
	fireVisit(t, w.AdvAcc.ID, tag, visitor)
	time.Sleep(8 * time.Second) // ample for a would-be second dispatch to land
	if n := dispatches(); n != 1 {
		t.Errorf("webhook dispatched %d times, want 1 — a repeat visit re-fired the enrolled event (first-enroll-only broken)", n)
	}
}
