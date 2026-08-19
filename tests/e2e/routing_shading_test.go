//go:build e2e

// Smart routing + bid shading tests. Both are statistical behaviors —
// they only manifest after enough auctions for the running stats to
// crystallise. The harness runs many auctions, then samples the debug
// endpoint to assert what the model learned.
package e2e

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestSmartRoutingTracksDSPStats — fire enough auctions that every DSP
// gets called at least once, then verify the router has stats for each.
// We don't assert an exact call count: the whole point of smart routing
// is to skip endpoints once their bid rate falls below threshold, so a
// hard "20 × 3 = 60 calls" check would break as the router gets smarter.
// Instead we assert (a) all 3 known DSPs appear in stats and (b) each
// has a positive TotalCalls — i.e. the RecordCall path is actually wired.
// Skip-threshold behavior is a separate test once a config-write helper
// lets us force a DSP into always-no-bid.
func TestSmartRoutingTracksDSPStats(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "routing")

	for i := 0; i < 20; i++ {
		h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "routing-user")
	}
	time.Sleep(500 * time.Millisecond) // win notifications are async

	stats := getRoutingStats(t, h)
	if len(stats) < 3 {
		t.Fatalf("smart router stats has %d DSPs, want >=3 (internal + 2 competitors); raw=%+v", len(stats), stats)
	}
	for _, s := range stats {
		if s.TotalCalls < 1 {
			t.Errorf("DSP %q TotalCalls=%d, want >=1 (RecordCall not firing for this endpoint)", s.DSPID, s.TotalCalls)
		}
	}
}

// TestSmartRoutingSkipsAlwaysNoBidDSP — proves the
// exchange.routing_min_calls threshold gates the skip: under the
// configured minimum the always-no-bid DSP is still included in
// fan-out (the router won't act on too-thin stats), and only after
// the threshold crosses does it get excluded.
//
// Complementary to TestCompetitiveB7 which asserts the steady-state
// behavior. This test checks the *transition* through the threshold.
func TestSmartRoutingSkipsAlwaysNoBidDSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	h.ResetSmartRouter(t)
	h.MakeDSPAlwaysNoBid(t, harness.PodDSPCompetitor1)
	h.RefreshAllCaches(t)

	// 10 auctions — well under default routing_min_calls=20.
	h.FireNAuctions(t, 10, "pl-news-mpu", "GBR", "mobile")
	preview := h.SmartRouterPreview(t)
	includedEarly := false
	for _, ep := range preview.Selected {
		if ep == h.URLs.ClusterDSPComp1 {
			includedEarly = true
			break
		}
	}
	if !includedEarly {
		t.Errorf("router skipped comp1 after only 10 calls; expected it to wait for routing_min_calls (default 20)")
	}

	// Train past the threshold and re-check — comp1 must drop now. This is
	// subtle under N exchange replicas: the skip rule needs the (display, comp1)
	// bucket to reach TotalCalls > routing_min_calls in the CLUSTER-GLOBAL
	// aggregate (events → NATS → ClickHouse dsp_calls → routing_reseed_interval),
	// but the skip is SELF-LIMITING — once a pod starts skipping comp1 (its local
	// count transiently crosses the threshold during a burst) it stops recording
	// comp1 calls, which can freeze the aggregate JUST BELOW the threshold. A
	// one-shot "fire 20 then poll" can strand comp1 at e.g. 17 recorded calls
	// forever: below min_calls, so the aggregate-driven preview never skips it,
	// yet nothing is firing to push it over.
	//
	// The escape is that at the sub-threshold frozen state NEITHER pod skips
	// (TotalCalls <= min_calls), so a FRESH auction is guaranteed to record a
	// comp1 call and climb the aggregate. So keep firing a small batch each poll
	// iteration until comp1 crosses the threshold and the reseed propagates the
	// skip to whichever pod the LB hands the preview to.
	dropped := func() bool {
		for _, ep := range h.SmartRouterPreview(t).Selected {
			if ep == h.URLs.ClusterDSPComp1 {
				return false
			}
		}
		return true
	}
	harness.WaitFor(t, 90*time.Second, "router to drop comp1 after enough no-bids (cross-replica reseed)", func() bool {
		if dropped() {
			return true
		}
		// Feed more no-bids; at the frozen sub-threshold state these DO record and
		// push the cluster aggregate past min_calls. Small batch so we don't blow
		// far past the threshold before the next reseed tick converges the pods.
		h.FireNAuctions(t, 6, "pl-news-mpu", "GBR", "mobile")
		return dropped()
	})
}

// TestBidShadingTrackerRecords — DSPs maintain a per-placement shading
// tracker (pkg/bidshading) that learns from win/loss outcomes. After
// enough auctions, the tracker should expose non-empty stats for the
// placement.
func TestBidShadingTrackerRecords(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "shading")

	for i := 0; i < 10; i++ {
		h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "shading-user")
	}
	time.Sleep(500 * time.Millisecond)

	// /v1/dsp/shading returns map[placementID]PlacementStats
	body := getDSPDebugBody(t, h, "/v1/dsp/shading")
	var stats map[string]map[string]any
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("shading decode: %v\n%s", err, string(body))
	}
	if len(stats) == 0 {
		t.Error("shading tracker has no placements recorded after 10 auctions")
	}
}

// --- helpers ---------------------------------------------------------------

type dspStat struct {
	DSPID      string `json:"DSPID"`
	BidRate    float64
	TotalCalls int64
	TotalBids  int64
}

func getRoutingStats(t *testing.T, h *harness.Harness) []dspStat {
	t.Helper()
	body := getExchangeDebugBody(t, h, routes.DebugExchangeRouting)
	var stats []dspStat
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("routing stats decode: %v\nraw: %s", err, string(body))
	}
	return stats
}

func getExchangeDebugBody(t *testing.T, h *harness.Harness, path string) []byte {
	return getURLBody(t, h, h.URLs.Exchange+path)
}

func getDSPDebugBody(t *testing.T, h *harness.Harness, path string) []byte {
	return getURLBody(t, h, h.URLs.DSP+path)
}

func getURLBody(t *testing.T, h *harness.Harness, url string) []byte {
	t.Helper()
	resp, err := h.HTTP.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	return body
}
