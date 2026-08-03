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

	// A shopper visits → enrolled → audience-rt emits retargeting.enrolled →
	// webhooks delivers it.
	fireVisit(t, w.AdvAcc.ID, tag, fmt.Sprintf("hook-visitor-%d", uniq))

	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE webhook_id=$1 AND event_type='retargeting.enrolled'`, hookID).Scan(&n); err != nil {
			t.Fatalf("delivery query: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retargeting.enrolled webhook was never delivered (no delivery attempt logged)")
		}
		time.Sleep(2 * time.Second)
	}
}
