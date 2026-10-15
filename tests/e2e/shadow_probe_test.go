//go:build e2e

// Non-blocking exploration probe (exchange.routing_shadow_probe), end to end.
//
// A skip-filtered slow DSP sampled by the ε-probe used to hold every probe
// auction to its response time. With the shadow probe on, the auction closes
// on the ranked DSPs and the probe's outcome is recorded after the fact —
// the router still learns, the auction doesn't pay.
package e2e

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// medianAuction runs n auctions and returns the median wall-clock time.
func medianAuction(t *testing.T, h *harness.Harness, tag string, n int) time.Duration {
	t.Helper()
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		h.RunAuction(t, "pl-news-mpu", "GBR", "mobile", fmt.Sprintf("user-shadow-%s-%d", tag, i))
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[n/2]
}

func TestShadowProbeDoesNotHoldAuction(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	h.ResetSmartRouter(t)
	h.MakeDSPAlwaysNoBid(t, harness.PodDSPCompetitor1)
	h.RefreshAllCaches(t)

	delayBefore := h.GetResolvedConfig(t, "dsp.response_delay", harness.PodDSPCompetitor1).Value
	if delayBefore == "" {
		delayBefore = "0s"
	}
	t.Cleanup(func() {
		h.DeleteConfig(t, "exchange.routing_explore_pct")
		h.DeleteConfig(t, "exchange.routing_shadow_probe")
		h.SetConfigForPod(t, "dsp.response_delay", delayBefore, harness.PodDSPCompetitor1)
	})

	// Train comp1 into a bid-rate skip while it's fast (quick training).
	h.SetConfigForPod(t, "dsp.response_delay", "0s", harness.PodDSPCompetitor1)
	h.FireNAuctions(t, 24, "pl-news-mpu", "GBR", "mobile")
	trainRouterUntilComp1Skipped(t, h)

	// Now make the skipped DSP slow and probe it on EVERY auction.
	h.SetConfigForPod(t, "dsp.response_delay", "350ms", harness.PodDSPCompetitor1)
	h.SetConfigForPod(t, "exchange.routing_explore_pct", "100", harness.PodExchange)
	h.SetConfigForPod(t, "exchange.routing_shadow_probe", "false", harness.PodExchange)
	time.Sleep(3 * time.Second) // config invalidate round-trip to every pod

	blocking := medianAuction(t, h, "blocking", 7)
	if blocking < 250*time.Millisecond {
		t.Fatalf("control: blocking probe median %v — expected the 350ms(±25%%) probe leg to hold the auction; is comp1 actually probed?", blocking)
	}

	h.SetConfigForPod(t, "exchange.routing_shadow_probe", "true", harness.PodExchange)
	time.Sleep(3 * time.Second)
	shadow := medianAuction(t, h, "shadow", 7)
	t.Logf("median auction: blocking probe %v, shadow probe %v", blocking, shadow)
	if shadow > blocking-150*time.Millisecond {
		t.Fatalf("shadow probe median %v not clearly below blocking %v — probe still holds the auction", shadow, blocking)
	}

	// The probe still happened and was recorded after the auction closed.
	re := regexp.MustCompile(`adtech_exchange_shadow_probe_total\{dsp_endpoint="` +
		regexp.QuoteMeta(h.URLs.ClusterDSPComp1) + `",outcome="late"\} (\S+)`)
	harness.WaitFor(t, 20*time.Second, "an exchange pod to report late shadow probes for comp1", func() bool {
		m := re.FindStringSubmatch(h.ExchangeMetricsText(t))
		if m == nil {
			return false
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		return v > 0
	})
}
