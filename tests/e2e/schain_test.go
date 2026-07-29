//go:build e2e

// SupplyChain (schain) transparency enforcement tests.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestSChainStrictRejectsMissing proves the exchange's schain gate
// (cmd/exchange/schain.go) end-to-end via a differential toggle: the SAME
// biddable auction is run with enforcement off (must win) and then strict
// (must no-bid), so the only variable is the gate.
//
// The request path is the EXTERNAL Prebid endpoint (PostPrebidAuctionRaw), not
// our SSP: ssp.seller_domain is now set (values.yaml), so the SSP always stamps
// a valid schain — a missing schain only ever reaches the exchange from an
// external source. The request carries an authorised site.domain so the (also
// strict) ads.txt gate passes and the ONLY variable is the schain mode.
func TestSChainStrictRejectsMissing(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "schain")
	const pod = "exchange-0"
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.schain_enforcement", "strict", pod) // restore the deployed default
		h.RefreshAllCaches(t)
	})

	// A schain-LESS bid request for the basic world's placement + authorised
	// publisher domain, geo/device matching its GBR/mobile campaign.
	noSchain := func() openrtb.BidRequest {
		return openrtb.BidRequest{
			ID:     "schain-missing",
			Imp:    []openrtb.Imp{{ID: "1", TagID: w.Placement.ID, BidFloor: 0.50, Banner: &openrtb.Banner{W: 300, H: 250}}},
			Site:   &openrtb.Site{Domain: w.Publisher.Domain, Publisher: &openrtb.Publisher{ID: w.Publisher.ID}},
			Device: &openrtb.Device{Geo: &openrtb.Geo{Country: "GBR"}, DeviceType: 1}, // mobile
			TMax:   500,
		}
	}

	// Baseline: enforcement off → the schain-less request still wins. Proves
	// demand exists, so a strict no-bid below can only be the gate.
	h.SetConfigForPod(t, "exchange.schain_enforcement", "off", pod)
	h.RefreshAllCaches(t)
	if resp, _ := h.PostPrebidAuctionRaw(t, noSchain()); resp.NoBid {
		t.Fatal("baseline (schain off): expected a winning bid for the schain-less request")
	}

	// Strict: the missing SupplyChain must now be rejected — and crucially the
	// no-bid must carry NBR=501, so it's provably the schain gate and not merely
	// absent demand.
	h.SetConfigForPod(t, "exchange.schain_enforcement", "strict", pod)
	h.RefreshAllCaches(t)
	got, _ := h.PostPrebidAuctionRaw(t, noSchain())
	if !got.NoBid {
		t.Error("strict schain: expected NoBid for a request carrying no SupplyChain")
	}
	if got.NBR != openrtb.NBRSChainInvalid {
		t.Errorf("strict schain: NBR = %d (%q), want %d (schain_invalid) — a block must be distinguishable from a genuine no-bid",
			got.NBR, got.NBRReason, openrtb.NBRSChainInvalid)
	}

	// Back to warn: the same request wins again — enforcement is genuinely
	// live-toggled, not a one-way ratchet.
	h.SetConfigForPod(t, "exchange.schain_enforcement", "warn", pod)
	h.RefreshAllCaches(t)
	if resp, _ := h.PostPrebidAuctionRaw(t, noSchain()); resp.NoBid {
		t.Error("warn schain: expected a winning bid (missing schain logged, not rejected)")
	}
}
