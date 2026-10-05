//go:build e2e

// External display adm (OpenRTB §4.3) end-to-end: an external bidder's
// display HTML — previously dropped (unknown creative → generic placeholder)
// — now serves verbatim, wrapped with the platform's signed beacons, while
// the ad server stays the frequency-cap authority (its cap decision precedes
// creative resolution, so the adm path can't bypass it — the headline
// negative here). Kill switch ssp.consume_display_adm reverts to legacy.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// externalDisplayFake wires a FakeDSP display bidder carrying HTML adm into
// the exchange fan-out (replacing the cluster DSPs for determinism) and
// restores the deployed endpoint set on cleanup.
func externalDisplayFake(t *testing.T, h *harness.Harness, adm string) *harness.FakeDSP {
	t.Helper()
	fake := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode: harness.FakeDSPBidder, BidPrice: 40.0, Seat: "fake-ext-display-seat",
		BidMutate: func(bid *openrtb.BidObj, req openrtb.BidRequest) {
			if adm != "" {
				bid.AdM = adm
			}
			bid.W, bid.H = 300, 250
			bid.ADomain = []string{"fakeext.example"}
		},
	})
	h.SetConfigForPod(t, "exchange.dsp_endpoints", fake.URL, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})
	return fake
}

var impURLRe = regexp.MustCompile(`src="([^"]*/v1/t/imp[^"]*)"`)

// TestDisplayAdMExternalWin: the buyer's HTML renders inside the platform
// wrapper with our signed impression pixel + viewability observer, §4.4
// macros substituted by the exchange — and the fired impression lands in
// analytics (zero slippage).
func TestDisplayAdMExternalWin(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dadm-win")

	buyerHTML := `<div data-buyer-brand="FAKEEXT" onclick="window.open('https://fakeext.example')">` +
		`EXTERNAL DISPLAY won @ ${AUCTION_PRICE} <img src="https://fakeext.example/own-pixel?a=${AUCTION_ID}"/></div>`
	externalDisplayFake(t, h, buyerHTML)

	var res harness.SSPServeResult
	harness.WaitFor(t, 20*time.Second, "external display adm serves", func() bool {
		res = h.ServeViaSSP(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: fmt.Sprintf("dadm-win-user-%d", time.Now().UnixNano()),
		})
		return !res.NoBid && strings.Contains(res.HTML, "FAKEEXT")
	})

	// Buyer HTML verbatim inside OUR wrapper, with OUR beacons.
	for _, want := range []string{
		`data-external-adm-wrapper="1"`,
		`data-buyer-brand="FAKEEXT"`,
		"/v1/t/imp",
		"IntersectionObserver",
	} {
		if !strings.Contains(res.HTML, want) {
			t.Errorf("served HTML missing %q:\n%s", want, res.HTML)
		}
	}
	// §4.4: the exchange substituted the auction macros into the buyer HTML.
	if strings.Contains(res.HTML, "${AUCTION") {
		t.Fatalf("literal auction macro served:\n%s", res.HTML)
	}
	if !strings.Contains(res.HTML, "won @ 40.0000") {
		t.Errorf("clearing price not substituted into buyer HTML:\n%s", res.HTML)
	}

	// Fire our injected impression like a browser — it must land downstream.
	m := impURLRe.FindStringSubmatch(res.HTML)
	if m == nil {
		t.Fatalf("no platform impression URL in served HTML:\n%s", res.HTML)
	}
	req, _ := http.NewRequest(http.MethodGet, strings.ReplaceAll(m[1], "&amp;", "&"), nil)
	browserHeaders(req)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("fire impression: %v", err)
	}
	resp.Body.Close()
	harness.WaitFor(t, 15*time.Second, "external adm impression recorded", func() bool {
		return h.ImpressionsByTrace(t, res.TraceID) >= 1
	})
}

// TestDisplayAdMFreqCap — the headline negative: the external-adm serve path
// still runs the ad server's DecideAndRecord, so the same user is capped
// after the default limit. (The fake ignores BAdv, so the SSP's bounded
// re-auction exhausts and returns an honest no-fill.)
func TestDisplayAdMFreqCap(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dadm-cap")
	externalDisplayFake(t, h, `<div data-buyer-brand="FAKEEXT">capped soon</div>`)

	// Distinct IPs per user (allowlisted ?ip= override): the display cap is
	// ALSO household-scoped (HMAC of client IP), so without this the second
	// user would be blocked by the first user's household counter — the
	// known one-IP display gotcha, not the thing under test.
	user := fmt.Sprintf("dadm-cap-user-%d", time.Now().UnixNano())
	serve := func() harness.SSPServeResult {
		return h.ServeViaSSP(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: user, IP: "203.0.113.77",
		})
	}
	// First serve may race the endpoint-config propagation — wait for the fake.
	harness.WaitFor(t, 20*time.Second, "external display fills", func() bool {
		r := serve()
		return !r.NoBid && strings.Contains(r.HTML, "FAKEEXT")
	})
	// Default cap is 5/user/campaign/day; serve until capped, bounded well
	// above the limit so a cap bypass fails loudly rather than looping.
	capped := false
	for i := 0; i < 10; i++ {
		if r := serve(); r.NoBid {
			capped = true
			break
		}
	}
	if !capped {
		t.Fatal("external-adm serves never hit the frequency cap — DecideAndRecord bypassed on the adm path")
	}
	// And a FRESH user in a FRESH household fills again (it's the cap, not a
	// general failure).
	fresh := h.ServeViaSSP(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
		UserID: user + "-fresh", IP: "203.0.113.89",
	})
	if fresh.NoBid || !strings.Contains(fresh.HTML, "FAKEEXT") {
		t.Fatalf("fresh user must fill after another user capped, got nobid=%v", fresh.NoBid)
	}
}

// TestDisplayAdMNoAdMPlaceholder (negative): an external winner WITHOUT adm
// keeps today's behavior — unknown creative → generic placeholder, no wrapper.
func TestDisplayAdMNoAdMPlaceholder(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dadm-noadm")
	externalDisplayFake(t, h, "") // bids, but no adm

	var res harness.SSPServeResult
	harness.WaitFor(t, 20*time.Second, "adm-less external winner serves placeholder", func() bool {
		res = h.ServeViaSSP(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: fmt.Sprintf("dadm-noadm-user-%d", time.Now().UnixNano()),
		})
		return !res.NoBid && strings.Contains(res.HTML, "Advertisement")
	})
	if strings.Contains(res.HTML, "data-external-adm-wrapper") {
		t.Errorf("adm-less winner must not ride the external wrapper:\n%s", res.HTML)
	}
}

// TestDisplayAdMInternalEmission (Phase B): the DSP emits bid.adm for a
// SELF-CONTAINED display creative (no ${...} platform macros) and omits it
// for the seeded macro-carrying template — and an internal winner still
// serves via the ad-server render path, never the external wrapper.
func TestDisplayAdMInternalEmission(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dadm-emit")

	admOfWinner := func(t *testing.T, r harness.AuctionResult) string {
		t.Helper()
		var br openrtb.BidResponse
		if err := json.Unmarshal(r.BidResponse, &br); err != nil {
			t.Fatalf("decode bid response: %v", err)
		}
		if br.NoBid || len(br.SeatBid) == 0 || len(br.SeatBid[0].Bid) == 0 {
			t.Fatal("auction did not fill")
		}
		return br.SeatBid[0].Bid[0].AdM
	}

	// Negative first: the seeded creative carries ${WIDTH}/${CAMPAIGN_ID}
	// platform macros → NOT self-contained → no adm emitted.
	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "dadm-emit-u1")
	if adm := admOfWinner(t, res); adm != "" {
		t.Fatalf("macro-carrying creative must emit no adm, got %q", adm)
	}

	// Flip the creative to self-contained HTML → adm emitted.
	selfContained := `<div data-self-contained="1" onclick="window.open('https://dadm-emit.test')">ACME self-contained 300x250</div>`
	h.SetCreativeHTML(t, w.AdvAcc, w.Campaign.CreativeID, selfContained)
	h.RefreshAllCaches(t)
	harness.WaitFor(t, 20*time.Second, "DSP emits display adm", func() bool {
		r := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", fmt.Sprintf("dadm-emit-u-%d", time.Now().UnixNano()))
		return strings.Contains(admOfWinner(t, r), "data-self-contained")
	})

	// Serving preference: the creative is KNOWN internally, so the serve
	// rides the ad-server render path — the buyer-wrapper div must NOT
	// appear even though the bid carried adm.
	serve := h.ServeViaSSP(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
		UserID: fmt.Sprintf("dadm-emit-serve-%d", time.Now().UnixNano()), IP: "203.0.113.42",
	})
	if serve.NoBid || !strings.Contains(serve.HTML, "data-self-contained") {
		t.Fatalf("internal winner must serve its creative, got nobid=%v html=%q", serve.NoBid, serve.HTML)
	}
	if strings.Contains(serve.HTML, "data-external-adm-wrapper") {
		t.Errorf("internal winner must use the render path, not the external wrapper:\n%s", serve.HTML)
	}
}

// TestDisplayAdMKillSwitch (negative): ssp.consume_display_adm=false reverts
// an adm-carrying external winner to the legacy placeholder.
func TestDisplayAdMKillSwitch(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dadm-kill")
	externalDisplayFake(t, h, `<div data-buyer-brand="FAKEEXT">should not serve</div>`)

	const pod = "ssp-0"
	const key = "ssp.consume_display_adm"
	t.Cleanup(func() { h.DeleteConfig(t, key) }) // revert to deployed default (true)
	h.SetConfigForPod(t, key, "false", pod)

	harness.WaitFor(t, 35*time.Second, "kill switch forces placeholder", func() bool {
		res := h.ServeViaSSP(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: fmt.Sprintf("dadm-kill-user-%d", time.Now().UnixNano()),
		})
		return !res.NoBid &&
			strings.Contains(res.HTML, "Advertisement") &&
			!strings.Contains(res.HTML, "FAKEEXT")
	})
}
