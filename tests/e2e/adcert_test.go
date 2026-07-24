//go:build e2e

// ads.cert (signed bid request) enforcement tests.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
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

// unsignedBidRequest is a minimal, well-formed OpenRTB bid request with NO
// source.ext.adcert signature. It only needs to parse — the ads.cert gate fires
// before targeting, so it never has to match a campaign.
func unsignedBidRequest(id string) string {
	return fmt.Sprintf(`{"id":%q,"imp":[{"id":"1","banner":{"w":300,"h":250},"bidfloor":0.5}],`+
		`"site":{"domain":"e2e-adcert.test"},"device":{"geo":{"country":"GBR"}},"cur":["USD"]}`, id)
}
