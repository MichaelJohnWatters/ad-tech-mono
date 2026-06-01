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

// TestSmartRoutingSkipsAlwaysNoBidDSP — requires a way to set a DSP to
// always return no_bid (e.g. via config-manager flip of its noise_pct
// to >100 with no_bid_rate 1.0). Skipped until the config-write helper
// lands.
func TestSmartRoutingSkipsAlwaysNoBidDSP(t *testing.T) {
	t.Skip("needs harness.SetConfig to force a DSP to no-bid; pending config-write helper")
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
	body := getExchangeDebugBody(t, h, "/v1/openrtb/routing")
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
