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

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestFraudDedupSameImpressionDropped — firing the same impression pixel
// twice with the same trace_id should result in only one NATS event
// reaching reporting. The tracker uses Redis SetNX to dedup.
func TestFraudDedupSameImpressionDropped(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "fraud-dedup")
	// Fresh committed accumulator so this campaign starts at 0 (the in-memory
	// billing state is long-lived + hydrated on boot).
	h.ResetBillingLedger(t)

	traceID := "fraud-dedup-trace-001"
	for i := 0; i < 5; i++ {
		// Fire the same impression 5 times. All 5 return 200 (the pixel
		// always responds), but only the first should reach the analytics
		// store + bill, via tracker dedup (Redis SetNX on trace_id).
		h.FireImpression(t, traceID,
			w.Campaign.ID, w.Campaign.CreativeID,
			w.Placement.ID, w.Publisher.ID, w.AdvAcc.ID,
			"USD", 1.50)
	}

	// committed spend is PER-CAMPAIGN, so this is isolated from other tests'
	// spend (unlike the global billing TotalSpend, which flaked here from async
	// spillover). 5 fires dedup to 1 → one impression at a $1.50 CPM realizes
	// 1.50/1000 = $0.0015 = 1500 micro-dollars.
	h.WaitCommittedMicros(t, w.Campaign.ID, harness.Micros(1.50/1000))
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

	// The config flip reaches each tracker replica via NATS invalidate +
	// its own poll — a fire can land on a replica that hasn't applied it
	// yet, so retry with fresh trace ids until one is strict-rejected
	// (same de-race as TestTrackerHMACStrictMode).
	var traceID string
	harness.WaitFor(t, 10*time.Second, "unsigned impression rejected under strict mode", func() bool {
		traceID = fmt.Sprintf("rej-hmac-%d", time.Now().UnixNano())
		resp := get(t, h, h.URLs.Tracker+"/v1/t/imp?tid="+traceID+"&cid="+w.Campaign.ID)
		resp.Body.Close()
		return resp.StatusCode == http.StatusForbidden
	})

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

// TestFraudIPBlocklistRejected — a DB-added IP in fraud_blocklists must cause
// the tracker's fraud check to block a request from that IP. The tracker now
// derives the client IP from X-Forwarded-For (behind Traefik), so the e2e can
// spoof it.
func TestFraudIPBlocklistRejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "fraud-ip")
	const badIP = "203.0.113.66"

	// Baseline: a clean IP is not blocked.
	if h.FireImpressionFromIP(t, "fraud-ip-baseline", w.Campaign.ID, "203.0.113.1") {
		t.Fatal("baseline IP should not be fraud-blocked")
	}

	h.AddFraudBlocklist(t, "ip", badIP)
	h.RefreshAllCaches(t)

	// RefreshAllCaches only reaches the ONE port-forwarded tracker pod, but
	// the tracker runs multiple replicas — a fire can land on a replica whose
	// fraud-rules warm cache hasn't polled/received the invalidate yet. Retry
	// with fresh trace ids until every path sees the row.
	// 70s: the invalidate usually lands in ms, but a tracker replica that
	// missed it falls back to the fraud-rules warm-cache poll
	// (cache.warm.fraud_rules.poll_interval, 60s) — the retry window must
	// cover a full poll cycle for every replica.
	harness.WaitFor(t, 70*time.Second, "blocklisted IP rejected by the tracker", func() bool {
		tid := fmt.Sprintf("fraud-ip-blocked-%d", time.Now().UnixNano())
		return h.FireImpressionFromIP(t, tid, w.Campaign.ID, badIP)
	})

	t.Cleanup(func() {
		h.ClearFraudBlocklist(t, "ip", badIP)
		h.RefreshAllCaches(t)
	})
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

	// Same multi-replica cache-propagation retry as the IP test above.
	harness.WaitFor(t, 70*time.Second, "blocklisted UA rejected by the tracker", func() bool {
		tid := fmt.Sprintf("fraud-ua-blocked-%d", time.Now().UnixNano())
		return h.FireImpressionUA(t, tid, w.Campaign.ID, "Mozilla/5.0 "+pattern+"/1.0")
	})

	// Cleanup the global row.
	h.ClearFraudBlocklist(t, "ua", pattern)
	h.RefreshAllCaches(t)
}

// TestFraudAdsTxtUnverifiedRejected — when a publisher's ads.txt doesn't
// list our platform as an authorised seller, strict enforcement makes the
// exchange no-bid the request before fan-out.
//
// Assumes the exchange pod id is "exchange-0" and BuildBasicWorld stamps the
// publisher domain as "e2e-<suffix>.test" (see world.go).
func TestFraudAdsTxtUnverifiedRejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "adstxt")
	const (
		domain = "e2e-adstxt.test"
		pod    = "exchange-0"
	)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.adstxt_enforcement", "off", pod)
		h.ClearAdsTxt(t, domain)
		h.RefreshAllCaches(t)
	})

	// Declare our seller identity.
	h.SetConfigForPod(t, "exchange.adstxt_seller_domain", "adtech.example", pod)
	h.SetConfigForPod(t, "exchange.adstxt_seller_id", "seat-1", pod)

	// Baseline: enforcement off → auction wins even without an ads.txt row.
	h.RefreshAllCaches(t)
	if h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "adstxt-u1")).NoBid {
		t.Fatal("baseline (enforcement off): expected a winning bid")
	}

	// Publisher publishes an ads.txt that does NOT list us → not_listed.
	h.SetAdsTxt(t, domain, []fraud.AdsTxtEntry{
		{Domain: "other-ssp.com", AccountID: "999", Relationship: "DIRECT"},
	}, "valid")
	h.SetConfigForPod(t, "exchange.adstxt_enforcement", "strict", pod)
	h.RefreshAllCaches(t)

	rejected := h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "adstxt-u2"))
	if !rejected.NoBid {
		t.Errorf("strict ads.txt: expected NoBid for a publisher whose ads.txt omits us")
	}
	// The block must be self-identifying — NBR=500 tells a genuine no-bid apart
	// from an ads.txt authorisation failure.
	if rejected.NBR != openrtb.NBRAdsTxtUnauthorised {
		t.Errorf("strict ads.txt: NBR = %d (%q), want %d (adstxt_not_authorised)",
			rejected.NBR, rejected.NBRReason, openrtb.NBRAdsTxtUnauthorised)
	}

	// Accept-under-strict: now the publisher authorises us (our seller_domain +
	// seller_id, DIRECT). Strict enforcement must let the same auction through —
	// proving strict isn't a blanket block, it's an authorisation check.
	h.SetAdsTxt(t, domain, []fraud.AdsTxtEntry{
		{Domain: "adtech.example", AccountID: "seat-1", Relationship: "DIRECT"},
	}, "valid")
	h.RefreshAllCaches(t)
	if h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "adstxt-u3")).NoBid {
		t.Errorf("strict ads.txt: expected a winning bid for a publisher that authorises us")
	}
}
