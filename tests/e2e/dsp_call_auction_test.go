//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestDSPCallTelemetryPerAuctionAndRollback — the exchange publishes per-DSP
// routing telemetry as ONE adtech.optimise.dsp_call_auction message per
// auction (default) and reporting expands it into per-leg dsp_calls rows;
// the rollback switch (exchange.dsp_call_per_auction=false) reverts to one
// message per leg. Either mode must land exactly ONE row per called DSP for
// the auction's trace (no lost leg, no duplicate), including the internal
// DSP — the shape the router reseed and the trace timeline depend on.
func TestDSPCallTelemetryPerAuctionAndRollback(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	h.RefreshAllCaches(t)
	t.Cleanup(func() { h.SetConfigForPod(t, "exchange.dsp_call_per_auction", "true", harness.PodExchange) })

	for _, mode := range []string{"true", "false"} {
		h.SetConfigForPod(t, "exchange.dsp_call_per_auction", mode, harness.PodExchange)
		res := h.RunAuction(t, "pl-news-mpu", "GBR", "mobile", "dspcall-auction-"+mode)
		tid := res.TraceID
		if tid == "" {
			t.Fatalf("per_auction=%s: auction returned no trace_id", mode)
		}
		where := fmt.Sprintf("FROM adtech.dsp_calls WHERE trace_id = '%s'", tid)
		harness.WaitFor(t, 40*time.Second, "dsp_calls rows for the trace (per_auction="+mode+")", func() bool {
			return h.ClickHouseScalar(t, "SELECT count() "+where) > 0
		})
		time.Sleep(2 * time.Second) // let any straggler/duplicate land before asserting exactness
		rows := h.ClickHouseScalar(t, "SELECT count() "+where)
		uniq := h.ClickHouseScalar(t, "SELECT uniqExact(dsp_endpoint) "+where)
		internal := h.ClickHouseScalar(t, "SELECT count() "+where+" AND dsp_endpoint LIKE '%dsp-internal%'")
		if rows != uniq {
			t.Errorf("per_auction=%s: %v rows for %v distinct DSPs — a leg was duplicated", mode, rows, uniq)
		}
		if internal != 1 {
			t.Errorf("per_auction=%s: internal DSP leg rows = %v, want exactly 1", mode, internal)
		}
	}
}
