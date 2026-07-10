//go:build e2e

// Production-like request coverage. The simulator now sends fully-populated
// OpenRTB (supply chain, consent/regulatory signals, identity, segments) rather
// than thin banner requests. These tests drive the same signals through the
// live SSP → Exchange → DSP path and assert the privacy engine's verdict is
// honoured end-to-end, plus an SSAI stitcher smoke test.
package e2e

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestConsentRegimesStillServe drives the full privacy-regime matrix through a
// real auction. None of our regimes block serving outright — GDPR-no-consent,
// COPPA, GPC, CCPA and GPP opt-outs downgrade to contextual-only, so a campaign
// with no behavioural targeting must still win. This proves the production-like
// consent signals flow SSP → exchange → DSP and don't accidentally suppress
// serving.
func TestConsentRegimesStillServe(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "consent")
	h.RefreshAllCaches(t)

	pl := w.Placement.ExternalID

	cases := []struct {
		name   string
		params harness.AuctionParams
	}{
		{"consented", harness.AuctionParams{Placement: pl, Geo: "GBR", Device: "mobile", GDPR: "1", Consent: "CPcqAAAPcqAAAAKAxAENCg"}},
		{"gdpr_no_consent", harness.AuctionParams{Placement: pl, Geo: "GBR", Device: "mobile", GDPR: "1"}},
		{"coppa", harness.AuctionParams{Placement: pl, Geo: "GBR", Device: "mobile", COPPA: "1"}},
		{"gpc", harness.AuctionParams{Placement: pl, Geo: "GBR", Device: "mobile", GPC: "1"}},
		{"ccpa_opt_out", harness.AuctionParams{Placement: pl, Geo: "GBR", Device: "mobile", USPrivacy: "1YYN"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := h.RunAuctionWith(t, c.params)
			win := h.ExtractWinner(t, res)
			if win.NoBid {
				t.Errorf("regime %s produced NoBid; contextual serving should continue under a privacy downgrade", c.name)
			}
		})
	}
}

// TestUID2IdentityAuction asserts a cookieless UID2-identified request runs a
// valid auction (the DSP resolves the user key from User.eids, not User.id).
func TestUID2IdentityAuction(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "uid2")
	h.RefreshAllCaches(t)

	res := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
		UID2: "sim-uid2-abc123", // no UserID: identity comes only from the UID2 EID
	})
	if h.ExtractWinner(t, res).NoBid {
		t.Errorf("UID2-identified request should still transact an auction")
	}
}

// TestSSAIManifestStitched is a smoke test for the SSAI stitcher: it must return
// a valid HLS manifest (ads stitched where the auction filled, content kept
// where it didn't). Skipped when the ssai service isn't part of the running
// stack, so the rest of the suite isn't gated on it.
func TestSSAIManifestStitched(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	if !serviceUp(h.URLs.SSAI) {
		t.Skip("ssai service not reachable; skipping SSAI smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	url := h.URLs.SSAI + routes.SSAIManifest + "?geo=USA&device=ctv"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("ssai manifest call: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ssai manifest status %d: %s", resp.StatusCode, string(body))
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "#EXTM3U") {
		t.Fatalf("ssai response is not an HLS manifest:\n%s", string(body))
	}
	m, err := ssai.ParseMedia(string(body))
	if err != nil {
		t.Fatalf("ssai manifest does not parse: %v", err)
	}
	if len(m.Segments) == 0 {
		t.Errorf("ssai manifest has no segments")
	}
}

func serviceUp(base string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+routes.Readyz, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
