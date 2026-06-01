//go:build e2e

// Fraud tests — IP blocklist, bot UA, ads.txt verification, and dedup.
// Dedup is the only one currently wired end-to-end; the others depend on
// fraud rules being loaded into the tracker/exchange from Postgres which
// isn't wired yet (rules are still hardcoded in pkg/fraud config).
package e2e

import (
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

