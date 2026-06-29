//go:build e2e

// Fraud tests — IP blocklist, bot UA, ads.txt verification, and dedup.
// Dedup is the only one currently wired end-to-end; the others depend on
// fraud rules being loaded into the tracker/exchange from Postgres which
// isn't wired yet (rules are still hardcoded in pkg/fraud config).
package e2e

import (
	"fmt"
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

	// Unique trace per run — the in-memory analytics store accumulates
	// across the whole reporting pod lifetime, so a deterministic
	// trace_id would mix in rejections from earlier runs of this same
	// test (e.g. 5 fires → 4 dedups, run twice → 8 dedups for the same
	// trace). Time-based suffix isolates each run cleanly.
	traceID := fmt.Sprintf("rej-dedup-%d", time.Now().UnixNano())
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

// TestFraudIPBlocklistRejected — IP blocklist is now DB-driven and enforced
// (see TestReplaceBlocklists_DBSourced for the unit-level proof), but the
// tracker derives the client IP from RemoteAddr, which an HTTP client can't
// set. Driving this end-to-end needs the tracker to honour X-Forwarded-For
// (it's behind Traefik anyway) or a dev IP-override header.
func TestFraudIPBlocklistRejected(t *testing.T) {
	t.Skip("IP blocklist is DB-wired + enforced, but the tracker reads RemoteAddr (not X-Forwarded-For), so an e2e client can't spoof the IP yet; covered by pkg/fraud unit test")
}

// TestFraudBotUARejected — a DB-added UA pattern must cause the tracker's
// fraud check to block. Uses a custom pattern not in the hardcoded bot list
// so it proves the fraud_blocklists warm-cache path specifically.
func TestFraudBotUARejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "fraud-ua")
	const pattern = "evil-e2e-bot"

	// Baseline: a normal UA is not blocked.
	if h.FireImpressionUA(t, "fraud-ua-baseline", w.Campaign.ID, "Mozilla/5.0 (e2e)") {
		t.Fatal("baseline UA should not be fraud-blocked")
	}

	h.AddFraudBlocklist(t, "ua", pattern)
	h.RefreshAllCaches(t)

	if !h.FireImpressionUA(t, "fraud-ua-blocked", w.Campaign.ID, "Mozilla/5.0 "+pattern+"/1.0") {
		t.Errorf("UA matching DB blocklist pattern %q should be fraud-blocked", pattern)
	}

	// Cleanup the global row.
	h.ClearFraudBlocklist(t, "ua", pattern)
	h.RefreshAllCaches(t)
}

// TestFraudAdsTxtUnverifiedRejected — ads.txt is crawled by cmd/adstxt
// but the exchange doesn't yet reject bid requests from unverified
// publishers.
func TestFraudAdsTxtUnverifiedRejected(t *testing.T) {
	t.Skip("exchange does not currently enforce ads.txt verification before accepting bid requests; pending")
}

