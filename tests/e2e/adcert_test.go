//go:build e2e

// ads.cert (signed bid request) enforcement tests.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestAdCertStrictRejectsUnsigned proves the DSP's ads.cert gate
// (cmd/dsp/adcert.go) both blocks a forged request and lets an authentic one
// through — the "accept + reject" pair, each with a machine-readable verdict.
//
// Preconditions baked into the stack (values.yaml): every DSP boots with
// DSP_ADCERT_KEY_URL pointing at the exchange's keyset, so it can verify the
// exchange's signatures. dsp.adcert_enforcement is live-tier and defaults off;
// this test flips it to strict on dsp-internal only.
//
//   - REJECT: a raw bid request POSTed straight to the DSP with NO ads.cert
//     signature. The gate runs before any targeting, so it no-bids with
//     NBR=502 (adcert_invalid) — provably a block, not "no campaign matched".
//   - ACCEPT: a full SSP→exchange→DSP auction. The exchange signs the outbound
//     request with its active key; the DSP verifies it against the fetched
//     keyset and bids. Proves strict rejects forgeries, not authentic traffic.
func TestAdCertStrictRejectsUnsigned(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "adcert")
	const pod = "dsp-internal-0"
	t.Cleanup(func() {
		h.SetConfigForPod(t, "dsp.adcert_enforcement", "off", pod)
		h.RefreshAllCaches(t)
	})

	// Baseline (enforcement off): an unsigned raw request is NOT blocked by the
	// adcert gate — any no-bid here is ordinary (no NBR=502). Confirms the gate
	// is genuinely what changes below.
	h.SetConfigForPod(t, "dsp.adcert_enforcement", "off", pod)
	h.RefreshAllCaches(t)
	base := h.PostOpenRTBBid(t, h.URLs.DSP, unsignedBidRequest("adcert-base"))
	if base.NBR == openrtb.NBRAdCertInvalid {
		t.Fatalf("baseline (adcert off): unsigned request blocked with NBR=502 — gate should be inert when off")
	}

	// REJECT: strict + unsigned → no-bid with the ads.cert reason code.
	h.SetConfigForPod(t, "dsp.adcert_enforcement", "strict", pod)
	h.RefreshAllCaches(t)
	rej := h.PostOpenRTBBid(t, h.URLs.DSP, unsignedBidRequest("adcert-reject"))
	if !rej.NoBid {
		t.Error("strict adcert: expected NoBid for an unsigned bid request")
	}
	if rej.NBR != openrtb.NBRAdCertInvalid {
		t.Errorf("strict adcert: NBR = %d (%q), want %d (adcert_invalid) — a forged request must be distinguishable from a genuine no-bid",
			rej.NBR, rej.NBRReason, openrtb.NBRAdCertInvalid)
	}

	// ACCEPT: the real signing path. The exchange signs; the DSP verifies against
	// the keyset it fetched at boot and bids. Strict must not block authentic
	// traffic. (Uses the full SSP path so the request is genuinely exchange-signed.)
	if h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "adcert-ok")).NoBid {
		t.Error("strict adcert: expected a winning bid for an authentically exchange-signed request")
	}
}

// TestAdCertBlockSurfacesInTrace proves a DSP-level enforcement block does NOT
// vanish into the exchange's aggregated no-bid — it lands in dsp_calls with its
// reason and shows up as a "DSP blocked" step in the (staff) trace timeline.
//
// It forces a REAL-auction block deterministically: the exchange signs the
// outbound request correctly, but with dsp.adcert_max_age set to 1ns the DSP
// reads that (validly-signed) request as stale and no-bids with reason
// adcert_stale. So the block comes through the genuine fan-out path (which emits
// the per-DSP DSPCallEvent), not a hand-POSTed request.
func TestAdCertBlockSurfacesInTrace(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "adcert-trace")
	pods := []string{"dsp-internal-0", "dsp-competitor1-0", "dsp-competitor2-0"}
	t.Cleanup(func() {
		for _, p := range pods {
			h.SetConfigForPod(t, "dsp.adcert_enforcement", "off", p)
			h.SetConfigForPod(t, "dsp.adcert_max_age", "5m", p)
		}
		h.RefreshAllCaches(t)
	})

	// Strict + a 1ns freshness window on every DSP → each reads the signed
	// request as stale and blocks. (Second-granularity signed timestamp means any
	// sub-second age exceeds 1ns, so this is reliably stale, never flaky.)
	for _, p := range pods {
		h.SetConfigForPod(t, "dsp.adcert_enforcement", "strict", p)
		h.SetConfigForPod(t, "dsp.adcert_max_age", "1ns", p)
	}
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "adcert-trace-u1")
	if !h.ExtractWinner(t, res).NoBid {
		t.Fatal("expected NoBid (all DSPs blocked on stale ads.cert)")
	}
	tid := res.TraceID
	if tid == "" {
		t.Fatal("auction returned no trace_id")
	}

	// 1) Anti-slippage: the reason is persisted per DSP in dsp_calls (via the
	// fire-and-forget DSPCallEvent → reporting → ClickHouse). Retry for the async
	// NATS→CH write.
	harness.WaitFor(t, 40*time.Second, "dsp_calls carries the adcert_stale reason", func() bool {
		return h.ClickHouseScalar(t, fmt.Sprintf(
			"SELECT count() FROM adtech.dsp_calls WHERE trace_id = '%s' AND no_bid_reason = 'adcert_stale'", tid)) > 0
	})

	// 2) The staff trace timeline surfaces the block with its reason.
	body := getTraceAsStaff(t, h, tid)
	if !strings.Contains(body, "DSP blocked") || !strings.Contains(body, "ads.cert") {
		t.Errorf("staff trace should show a 'DSP blocked: ads.cert…' step; got:\n%s", body)
	}
}

// getTraceAsStaff calls reporting's scoped trace endpoint with staff headers
// (staff is unscoped → sees every event) and returns the raw JSON body.
func getTraceAsStaff(t *testing.T, h *harness.Harness, traceID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.URLs.Reporting+routes.ReportingTrace+"?trace_id="+traceID, nil)
	if err != nil {
		t.Fatalf("build trace request: %v", err)
	}
	req.Header.Set(constants.HeaderAccountType, "staff")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("trace call failed: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trace status %d: %s", resp.StatusCode, string(b))
	}
	return string(b)
}

// unsignedBidRequest is a minimal, well-formed OpenRTB bid request with NO
// source.ext.adcert signature. It only needs to parse — the ads.cert gate fires
// before targeting, so it never has to match a campaign.
func unsignedBidRequest(id string) string {
	return fmt.Sprintf(`{"id":%q,"imp":[{"id":"1","banner":{"w":300,"h":250},"bidfloor":0.5}],`+
		`"site":{"domain":"e2e-adcert.test"},"device":{"geo":{"country":"GBR"}},"cur":["USD"]}`, id)
}
