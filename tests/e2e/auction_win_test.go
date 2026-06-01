//go:build e2e

// Auction.win NATS event tests.
//
// adtech.auction.win is published by the exchange after every auction. Until
// 2026-05-31 it had no subscribers and went to /dev/null. Reporting now
// consumes it for analytics (and, in a future migration, will write it to
// the billing ledger as the source of truth for spend — see PLAN.md).
//
// DSP budget tracking continues to go through the OpenRTB HTTP win-notify
// path (uniform across internal and external DSPs); the NATS event is for
// non-DSP consumers only. This file tests the reporting analytics path.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestAuctionWinLandsInAnalytics — every auction that produces a winner
// should produce exactly one record in the reporting analytics store. Zero
// = NATS subscriber wiring broken / event dropped. >1 = dedup broken /
// handler running twice.
func TestAuctionWinLandsInAnalytics(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "aw-analytics")

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "aw-user-1")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected a winning bid for analytics test; got no_bid")
	}

	// NATS publish + reporting consume runs async after the auction HTTP
	// response returns. Usually ~50ms; bursty test workloads can stretch
	// to a few seconds when JetStream consumer fetch batches.
	harness.WaitFor(t, 10*time.Second, "auction.win to land in analytics", func() bool {
		return h.AuctionWinCount(t, res.TraceID) >= 1
	})

	// Once it lands, the count must be exactly 1 — assert dedup. We give
	// the dedup another moment in case a duplicate publish was in-flight.
	time.Sleep(200 * time.Millisecond)
	if c := h.AuctionWinCount(t, res.TraceID); c != 1 {
		t.Errorf("auction win count for trace %q = %d, want exactly 1", res.TraceID, c)
	}
}

