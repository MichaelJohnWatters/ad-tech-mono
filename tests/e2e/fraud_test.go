//go:build e2e

// Fraud tests — IP blocklist, bot UA, ads.txt verification, and dedup.
// Dedup is the only one currently wired end-to-end; the others depend on
// fraud rules being loaded into the tracker/exchange from Postgres which
// isn't wired yet (rules are still hardcoded in pkg/fraud config).
package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestFraudDedupSameImpressionDropped — firing the same impression pixel
// twice with the same trace_id should result in only one NATS event
// reaching reporting. The tracker uses Redis SetNX to dedup.
func TestFraudDedupSameImpressionDropped(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "fraud-dedup")

	// Snapshot TotalSpend before — the reporting service's billing ledger
	// is in-memory and accumulates across the whole test suite (harness.Reset
	// truncates Postgres + flushes Redis but doesn't touch the in-process
	// ledger). We measure delta instead of absolute so test ordering doesn't
	// matter.
	spendBefore := totalSpend(t, h)

	traceID := "fraud-dedup-trace-001"
	for i := 0; i < 5; i++ {
		// Fire the same impression 5 times. All 5 return 200 (the pixel
		// always responds), but only the first should reach the analytics
		// store via tracker dedup (SetNX on trace_id).
		h.FireImpression(t, traceID,
			w.Campaign.ID, w.Campaign.CreativeID,
			w.Placement.ID, w.Publisher.ID, w.AdvAcc.ID,
			"USD", 1.50)
	}

	// Give the async NATS publish time to flow through.
	time.Sleep(1 * time.Second)

	delta := totalSpend(t, h) - spendBefore
	// One impression at 1.50, allow a tiny tolerance for float rounding.
	if delta > 1.51 || delta < 1.49 {
		t.Errorf("TotalSpend delta = %.4f, want ~1.50 (5 fires with same trace_id should dedup to 1)", delta)
	}
}

// TestTrackerRejectedEventDedup — the dedup test already asserts the
// billing side (deduped impression doesn't bill). Here we assert the
// negative-signal side: the dropped impressions publish
// adtech.tracker.rejected with reason="dedup" so reporting can count
// them. 5 fires → 1 lands as impression + 4 land as rejected.
func TestTrackerRejectedEventDedup(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rej-dedup")

	traceID := "rej-dedup-trace-001"
	for i := 0; i < 5; i++ {
		h.FireImpression(t, traceID,
			w.Campaign.ID, w.Campaign.CreativeID,
			w.Placement.ID, w.Publisher.ID, w.AdvAcc.ID,
			"USD", 1.50)
	}

	harness.WaitFor(t, 5*time.Second, "dedup rejection events recorded", func() bool {
		return len(h.TrackerRejectionsByTrace(t, traceID, "dedup")) >= 4
	})

	rejections := h.TrackerRejectionsByTrace(t, traceID, "dedup")
	if got, want := len(rejections), 4; got != want {
		t.Errorf("dedup rejections = %d, want %d (5 fires - 1 first-seen = 4 deduped)", got, want)
	}
	for _, r := range rejections {
		if r.EventType != "impression" {
			t.Errorf("EventType = %q, want impression", r.EventType)
		}
		if r.Reason != "dedup" {
			t.Errorf("Reason = %q, want dedup", r.Reason)
		}
	}
}

// TestTrackerRejectedEventHMACStrict — flip
// tracker.signature_validation=true, hit /v1/t/imp without a sig (403),
// assert the rejected event lands with reason=invalid_signature.
// Cleanup mirrors TestTrackerHMACStrictMode so we don't poison
// downstream tests.
func TestTrackerRejectedEventHMACStrict(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rej-hmac")

	const pod = "tracker-0"
	const key = "tracker.signature_validation"
	t.Cleanup(func() {
		h.SetConfigForPod(t, key, "false", pod)
	})
	h.SetConfigForPod(t, key, "true", pod)

	traceID := "rej-hmac-trace-001"
	url := h.URLs.Tracker + "/v1/t/imp?tid=" + traceID + "&cid=" + w.Campaign.ID
	resp := get(t, h, url)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("strict-mode unsigned status = %d, want 403", resp.StatusCode)
	}

	harness.WaitFor(t, 5*time.Second, "invalid_signature rejection recorded", func() bool {
		return len(h.TrackerRejectionsByTrace(t, traceID, "invalid_signature")) >= 1
	})
}

func totalSpend(t *testing.T, h *harness.Harness) float64 {
	t.Helper()
	s := h.BillingSummary(t)
	v, ok := s["TotalSpend"].(float64)
	if !ok {
		t.Fatalf("billing summary missing TotalSpend (float64); got %#v", s["TotalSpend"])
	}
	return v
}

// TestFraudIPBlocklistRejected — pending fraud rules table being wired.
// pkg/fraud has a real-time checker but its IP/UA blocklists are loaded
// from in-code defaults (pkg/fraud/lists), not from Postgres.
func TestFraudIPBlocklistRejected(t *testing.T) {
	t.Skip("fraud blocklists are currently in-code (pkg/fraud/lists); test ready when blocklist warm-cache lands")
}

// TestFraudBotUARejected — same.
func TestFraudBotUARejected(t *testing.T) {
	t.Skip("UA blocklist is in-code; pending warm-cache wiring")
}

// TestFraudAdsTxtUnverifiedRejected — ads.txt is crawled by cmd/adstxt
// but the exchange doesn't yet reject bid requests from unverified
// publishers.
func TestFraudAdsTxtUnverifiedRejected(t *testing.T) {
	t.Skip("exchange does not currently enforce ads.txt verification before accepting bid requests; pending")
}

