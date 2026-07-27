//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// Pod IDs as configured in the Tiltfile. Hardcoded here so the
// competitive-auction tests have a stable reference for SetConfigForPod
// targets. If the Tiltfile POD_NAME values ever change, update these in
// lockstep.
const (
	PodDSPInternal    = "dsp-internal-0"
	PodDSPCompetitor1 = "dsp-competitor1-0"
	PodDSPCompetitor2 = "dsp-competitor2-0"
	PodExchange       = "exchange-0"
)

// WithDeterministicCompetitors zeroes the noise and random-no-bid knobs on
// both competitor DSPs so a competitive auction test can rely on each DSP
// returning its actual base_bid × modifiers — no ±30% jitter, no random
// drops. Returns immediately; the change is in effect after the NATS
// invalidate delivery (~ms).
//
// Registers t.Cleanup to restore the pod defaults (30% noise / 20% no-bid
// for comp1, 40% noise / 15% no-bid for comp2) so subsequent tests that
// expect realistic competitor behaviour aren't poisoned.
func (h *Harness) WithDeterministicCompetitors(t *testing.T) {
	t.Helper()
	h.SetConfigForPod(t, "dsp.noise_pct", "0", PodDSPCompetitor1)
	h.SetConfigForPod(t, "dsp.no_bid_rate", "0", PodDSPCompetitor1)
	h.SetConfigForPod(t, "dsp.noise_pct", "0", PodDSPCompetitor2)
	h.SetConfigForPod(t, "dsp.no_bid_rate", "0", PodDSPCompetitor2)

	t.Cleanup(func() {
		h.SetConfigForPod(t, "dsp.noise_pct", "30", PodDSPCompetitor1)
		h.SetConfigForPod(t, "dsp.no_bid_rate", "0.2", PodDSPCompetitor1)
		h.SetConfigForPod(t, "dsp.noise_pct", "40", PodDSPCompetitor2)
		h.SetConfigForPod(t, "dsp.no_bid_rate", "0.15", PodDSPCompetitor2)
	})
}

// MakeDSPAlwaysNoBid sets a DSP pod's no_bid_rate to 1.0 so it returns
// no_bid for every request, regardless of campaign matches. Used to test
// "1 DSP no-bids, others compete" and "all DSPs no-bid" scenarios.
// Restored on t.Cleanup.
func (h *Harness) MakeDSPAlwaysNoBid(t *testing.T, podID string) {
	t.Helper()
	// Snapshot-and-restore, NOT restore-to-zero: competitor pods carry
	// seeded per-pod no_bid_rate values (comp1=0.2, comp2=0.15), and the
	// old hardcoded "0" restore clobbered them — TestSeedDefaults then
	// failed on any stack where a routing test had ever run.
	before := h.GetResolvedConfig(t, "dsp.no_bid_rate", podID).Value
	if before == "" {
		before = "0"
	}
	h.SetConfigForPod(t, "dsp.no_bid_rate", "1.0", podID)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "dsp.no_bid_rate", before, podID)
	})
}

// SetExchangeBidTimeout overrides exchange.bid_timeout for the test (e.g.
// "100ms" to make the timeout fire faster). Restored on t.Cleanup to the
// platform default of 500ms.
func (h *Harness) SetExchangeBidTimeout(t *testing.T, dur string) {
	t.Helper()
	h.SetConfigForPod(t, "exchange.bid_timeout", dur, PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.bid_timeout", "500ms", PodExchange)
	})
}

// RunAuctionWithSlowDSPs is RunAuction but sends the X-Dev-Slow-DSPs header
// to the SSP, which forwards it to the exchange. Each integer in the CSV
// is a 0-indexed DSP that the exchange should deliberately stall on for
// this auction (so the bid_timeout path is exercised). Empty string runs
// every DSP normally.
//
// Used by competitive auction tests that need to prove slow DSPs are cut
// off without the test depending on actual network latency.
func (h *Harness) RunAuctionWithSlowDSPs(t *testing.T, placementExternalID, geo, device, userID, slowCSV string) AuctionResult {
	t.Helper()
	q := []string{}
	add := func(k, v string) {
		if v != "" {
			q = append(q, k+"="+v)
		}
	}
	add("placement_id", placementExternalID)
	add("geo", geo)
	add("device", device)
	add("user_id", userID)

	url := h.URLs.SSP + "/v1/ssp/request"
	if len(q) > 0 {
		url += "?" + strings.Join(q, "&")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("auction request: %v", err)
	}
	if slowCSV != "" {
		req.Header.Set("X-Dev-Slow-DSPs", slowCSV)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("auction call: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auction status %d: %s", resp.StatusCode, string(body))
	}
	var out AuctionResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("auction decode: %v\nbody: %s", err, string(body))
	}
	return out
}

// SeedStandard re-runs the standard seed profile to populate full multi-DSP
// inventory: 4 internal campaigns + 3 comp1 campaigns + 4 comp2 campaigns +
// 6 publishers with placements. Used by cross-DSP auction tests that need a
// realistic competitive set without rebuilding it row-by-row.
//
// Caller is responsible for refreshing caches afterwards.
func (h *Harness) SeedStandard(t *testing.T) {
	t.Helper()
	url := h.URLs.Gateway + routes.DevResetReseed
	// h.HTTP's 10s Timeout caps requests regardless of context deadline, and
	// a full truncate+reseed takes ~6s idle — over 10s under suite load — so
	// use a dedicated client with a longer cap. Retry transport errors like
	// refreshOne does: the gateway port-forward flaps (EOF) under load, and
	// the reseed is idempotent so re-POSTing is safe.
	client := &http.Client{Timeout: 60 * time.Second}
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		resp, err = client.Post(url, "application/json", nil)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("reseed call after retries: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("reseed status %d: %s", resp.StatusCode, string(body))
	}
}

// FireNAuctions runs the same auction N times. Used by smart-router tests
// that need to "teach" the router by repeated history before asserting on
// its filtering behavior, and by budget-exhaustion tests that need to burn
// through a daily cap. Returns the per-call winner so the caller can do
// per-run assertions if needed.
func (h *Harness) FireNAuctions(t *testing.T, n int, placement, geo, device string) []BidResponseWinner {
	t.Helper()
	out := make([]BidResponseWinner, 0, n)
	for i := 0; i < n; i++ {
		r := h.RunAuction(t, placement, geo, device, fmt.Sprintf("user-burn-%d", i))
		out = append(out, h.ExtractWinner(t, r))
	}
	return out
}

// ResetSmartRouter wipes the exchange's smart-router learned state via the
// debug endpoint. Used at the top of router tests so a deterministic
// training sequence isn't polluted by stats from earlier tests in the
// same Tilt process.
func (h *Harness) ResetSmartRouter(t *testing.T) {
	t.Helper()
	// Retry transport flaps: this runs at the TOP of router tests, so a
	// single-shot failure here fast-fails the whole test in setup.
	resp, err := h.getWithRetry(h.URLs.Exchange + routes.DebugExchangeRouting + "?reset=true")
	if err != nil {
		t.Fatalf("router reset call after retries: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("router reset status %d: %s", resp.StatusCode, string(body))
	}
}

// RouterPreview is the parsed response from /debug/exchange/routing?preview=true.
// `Selected` is the list SmartRouter.SelectDSPs would return right now;
// `All` is the full configured endpoint list (for diff/visibility).
type RouterPreview struct {
	Channel  string   `json:"channel"`
	All      []string `json:"all"`
	Selected []string `json:"selected"`
}

// SmartRouterPreview asks the exchange "given current learned state, which
// DSPs would you fan a request to?". Used to assert that a DSP the
// router has learned is always-no-bid gets excluded from the next auction's
// fan-out.
func (h *Harness) SmartRouterPreview(t *testing.T) RouterPreview {
	t.Helper()
	// Retry transport flaps: this is called REPEATEDLY inside WaitFor loops,
	// so one blip mid-wait would otherwise fast-fail the test.
	resp, err := h.getWithRetry(h.URLs.Exchange + routes.DebugExchangeRouting + "?preview=true")
	if err != nil {
		t.Fatalf("router preview call after retries: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("router preview status %d: %s", resp.StatusCode, string(body))
	}
	var rp RouterPreview
	if err := json.Unmarshal(body, &rp); err != nil {
		t.Fatalf("router preview decode: %v\nbody: %s", err, string(body))
	}
	return rp
}
