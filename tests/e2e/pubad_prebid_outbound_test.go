//go:build e2e

// Publisher-adserver → external Prebid Server outbound. Verifies the third
// pillar of our Prebid integration: when pubad falls through to programmatic,
// it fans out to our own SSP AND any configured external Prebid Servers in
// parallel, then picks the highest clearing price across all of them.
//
// Inbound (us as bidder) is covered by tests/e2e/prebid_test.go.
// Pub sim Prebid mode (browser-driven, no server work) is covered by
// web/templates/simulator/minimal.html.
package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestPubAdOutboundPrebidBeatsSSP — wires pubad to a fake Prebid Server
// bidding at 25.00 CPM. BasicWorld's SSP-side campaign bids ~3.50. With
// both demand sources running in parallel, pubad must pick the higher
// (Prebid) price and render it.
//
// Proves: pubad's price-comparison across demand sources works, and the
// Prebid-winner render path (bid.adm verbatim, no ad-server hop) fires.
func TestPubAdOutboundPrebidBeatsSSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pubad-pb")

	// Fake external Prebid Server bidding at 25.00 with a recognisable
	// HTML marker so we can prove the Prebid bid was the one rendered.
	prebidStub := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode:     harness.FakeDSPBidder,
		BidPrice: 25.00,
		Seat:     "external-prebid-seat",
	})

	// FakeDSP serves OpenRTB at /v1/openrtb/bid. Point pubad's outbound
	// Prebid client at that path — wire format is identical to a real
	// Prebid Server, only the URL differs.
	h.SetConfigForPod(t, "publisher_adserver.prebid_servers",
		prebidStub.URL+"/v1/openrtb/bid", "publisher-adserver-0")
	t.Cleanup(func() {
		h.SetConfigForPod(t, "publisher_adserver.prebid_servers", "",
			"publisher-adserver-0")
	})

	resp := h.ServePubAdRaw(t, "placement_id="+w.Placement.ExternalID+"&geo=GBR&device=mobile")
	if resp.NoBid {
		t.Fatalf("expected a winning bid (SSP and Prebid both bid); got nobid (resp=%+v)", resp)
	}
	if resp.Source != "prebid" {
		t.Fatalf("source = %q; want prebid (25.00 should beat SSP's ~3.50)", resp.Source)
	}
	if resp.ClearingPrice < 20.0 {
		t.Errorf("clearing_price = %v; want close to 25.00 (the fake's bid)", resp.ClearingPrice)
	}
	if prebidStub.BidCalls() == 0 {
		t.Errorf("fake Prebid Server never called — pubad didn't fan out")
	}
}

// TestPubAdOutboundPrebidLosesToSSP — fake Prebid bids 0.50; SSP's
// campaign bids ~3.50. SSP must win. Proves the comparison is bidirectional
// (not "Prebid always wins" — actual price wins).
func TestPubAdOutboundPrebidLosesToSSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pubad-pb-lose")

	prebidStub := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode:     harness.FakeDSPBidder,
		BidPrice: 0.50, // below SSP campaign's ~3.50
		Seat:     "external-prebid-seat",
	})

	h.SetConfigForPod(t, "publisher_adserver.prebid_servers",
		prebidStub.URL+"/v1/openrtb/bid", "publisher-adserver-0")
	t.Cleanup(func() {
		h.SetConfigForPod(t, "publisher_adserver.prebid_servers", "",
			"publisher-adserver-0")
	})

	resp := h.ServePubAdRaw(t, "placement_id="+w.Placement.ExternalID+"&geo=GBR&device=mobile")
	if resp.NoBid {
		t.Fatal("expected SSP to win; got nobid")
	}
	if resp.Source == "prebid" {
		t.Errorf("source = prebid; want SSP-pass-through (0.50 should NOT beat 3.50)")
	}
	if prebidStub.BidCalls() == 0 {
		t.Errorf("fake Prebid Server never called — pubad didn't fan out")
	}
	// SSP-pass-through response: HTML should contain the BasicWorld e2e
	// creative marker, not anything from the fake.
	if !strings.Contains(resp.HTML, "via e2e") {
		t.Errorf("HTML missing SSP marker; got %s", resp.HTML)
	}
}

// TestPubAdOutboundPrebidNoBidFallsThroughToSSP — Prebid Server returns
// no_bid; SSP still wins normally. Proves a Prebid-side no_bid doesn't
// suppress the SSP's bid.
func TestPubAdOutboundPrebidNoBidFallsThroughToSSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pubad-pb-nobid")

	prebidStub := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode:     harness.FakeDSPBidder,
		BidPrice: 0, // BidPrice=0 → fake returns NoBid (see FakeDSP bidder mode)
	})

	h.SetConfigForPod(t, "publisher_adserver.prebid_servers",
		prebidStub.URL+"/v1/openrtb/bid", "publisher-adserver-0")
	t.Cleanup(func() {
		h.SetConfigForPod(t, "publisher_adserver.prebid_servers", "",
			"publisher-adserver-0")
	})

	resp := h.ServePubAdRaw(t, "placement_id="+w.Placement.ExternalID+"&geo=GBR&device=mobile")
	if resp.NoBid {
		t.Fatal("expected SSP to win; got nobid")
	}
	if resp.Source == "prebid" {
		t.Errorf("source = prebid; Prebid no_bid should have left SSP as winner")
	}
}

// TestPubAdOutboundPrebidViewabilityBeaconFires — the zero-data-slippage fix:
// a winning external Prebid render must carry OUR viewability beacon, not just
// the impression pixel, so we observe whether the external creative was
// actually viewable (fill-rate + vCPM depend on it). Proves the wrapper injects
// the IntersectionObserver + signed /v1/t/view, and that firing that signed URL
// with the client-measured dur/pct/area appended yields an IAB-viewable view
// (i.e. the tracker's signature validation excludes the measured params).
func TestPubAdOutboundPrebidViewabilityBeaconFires(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pubad-pb-view")

	prebidStub := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode:     harness.FakeDSPBidder,
		BidPrice: 25.00,
		Seat:     "external-view-seat",
	})
	h.SetConfigForPod(t, "publisher_adserver.prebid_servers",
		prebidStub.URL+"/v1/openrtb/bid", "publisher-adserver-0")
	t.Cleanup(func() {
		h.SetConfigForPod(t, "publisher_adserver.prebid_servers", "", "publisher-adserver-0")
	})

	resp := h.ServePubAdRaw(t, "placement_id="+w.Placement.ExternalID+"&geo=GBR&device=mobile")
	if resp.Source != "prebid" {
		t.Fatalf("source = %q; want prebid", resp.Source)
	}

	// The wrapped HTML must carry BOTH the impression pixel and the injected
	// viewability observer (was: pixel only → the view never fired).
	if !strings.Contains(resp.HTML, "/v1/t/imp") {
		t.Error("wrapped HTML missing the impression pixel")
	}
	if !strings.Contains(resp.HTML, "IntersectionObserver") || !strings.Contains(resp.HTML, "/v1/t/view") {
		t.Errorf("wrapped HTML missing the injected viewability beacon; html=%q", resp.HTML)
	}
	if resp.ViewabilityURL == "" {
		t.Fatal("response carried no viewability URL to fire")
	}

	// Fire the signed beacon URL exactly as the browser would — with the
	// measured dur/pct/area appended after signing — and require an IAB view.
	// (The tracker excludes those measured params from signature validation, so
	// the signed beacon still validates under strict signing.)
	if !h.FireViewURL(t, resp.ViewabilityURL+"&dur=1500&pct=80&area=90000") {
		t.Error("the injected viewability beacon did not produce an IAB-viewable view")
	}
}
