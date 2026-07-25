//go:build e2e

// Live smart-routing knobs + config-deletion revert, end to end.
//
// Covers two behaviours against the deployed stack:
//
//  1. The exchange.routing_* knobs are LIVE: flipping them through the
//     config API changes the router's fan-out decision within the NATS
//     invalidate round-trip — no restart.
//  2. DELETING a live config row reverts every pod to the default. This
//     regressed silently for a long time: the poll only applied rows that
//     existed, so a deleted row pinned the last-known value in every
//     running pod until restart.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// comp1Selected reports whether dsp-competitor1 is in the router's current
// fan-out preview.
func comp1Selected(t *testing.T, h *harness.Harness) bool {
	t.Helper()
	for _, ep := range h.SmartRouterPreview(t).Selected {
		if ep == h.URLs.ClusterDSPComp1 {
			return true
		}
	}
	return false
}

func TestRoutingKnobsLiveAndConfigDeleteReverts(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	h.ResetSmartRouter(t)
	h.MakeDSPAlwaysNoBid(t, harness.PodDSPCompetitor1)
	h.RefreshAllCaches(t)

	// Whatever happens mid-test, leave no routing knob rows behind —
	// deletion reverts to schema defaults (that revert is itself the
	// feature under test, so cleanup doubles as a sanity check).
	t.Cleanup(func() {
		h.DeleteConfig(t, "exchange.routing_enabled")
		h.DeleteConfig(t, "exchange.routing_min_calls")
	})

	// Train comp1 into a skip: 30 no-bid calls, converged cluster-wide by
	// the reseed (events → ClickHouse → routing_reseed_interval).
	h.FireNAuctions(t, 30, "pl-news-mpu", "GBR", "mobile")
	harness.WaitFor(t, 30*time.Second, "router to skip comp1 after training", func() bool {
		return !comp1Selected(t, h)
	})

	// Knob 1 — kill-switch. routing_enabled=false must bypass the learned
	// skip and fan out to everyone, live.
	h.SetConfigForPod(t, "exchange.routing_enabled", "false", harness.PodExchange)
	harness.WaitFor(t, 15*time.Second, "kill-switch to include comp1", func() bool {
		return comp1Selected(t, h)
	})

	// Deleting the row must revert routing_enabled to its default (true)
	// on every exchange pod — the learned skip comes back WITHOUT anyone
	// writing "true" explicitly. This is the deleted-row-pins-stale-value
	// regression test.
	h.DeleteConfig(t, "exchange.routing_enabled")
	harness.WaitFor(t, 45*time.Second, "config delete to revert kill-switch (comp1 skipped again)", func() bool {
		return !comp1Selected(t, h)
	})

	// Knob 2 — routing_min_calls (the key that used to be silently
	// ignored). Raising it above the training volume un-skips comp1:
	// 30 calls is thin evidence against a 100-call floor.
	h.SetConfigForPod(t, "exchange.routing_min_calls", "100", harness.PodExchange)
	harness.WaitFor(t, 15*time.Second, "min_calls=100 to include comp1 (thin evidence)", func() bool {
		return comp1Selected(t, h)
	})

	// And deleting that row reverts to the schema default (20) — skip again.
	h.DeleteConfig(t, "exchange.routing_min_calls")
	harness.WaitFor(t, 45*time.Second, "config delete to revert min_calls (comp1 skipped again)", func() bool {
		return !comp1Selected(t, h)
	})
}
