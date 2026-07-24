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
// Why this is deterministic: the local SSP has no ssp.seller_domain configured
// (static tier, empty in values.yaml), so it originates NO SupplyChain on the
// outbound bid request. Under strict enforcement a request with no schain is
// rejected before fan-out; under off it proceeds and the BuildBasicWorld
// campaign bids. If the SSP ever started stamping a valid schain, the strict
// case would win and this test would fail loudly — which is the correct signal
// that the assumption changed, not a silent pass.
//
// The complementary "a VALID schain is accepted under strict" direction depends
// on the SSP originating a chain (seller_domain set) and is covered by unit
// tests: pkg/openrtb (ValidateSChain, SChainOf) + cmd/ssp (originSChain).
func TestSChainStrictRejectsMissing(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "schain")
	const pod = "exchange-0"
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.schain_enforcement", "warn", pod)
		h.RefreshAllCaches(t)
	})

	// Baseline: enforcement off → the schain-less request still wins. Proves
	// demand exists, so a strict no-bid below can only be the gate.
	h.SetConfigForPod(t, "exchange.schain_enforcement", "off", pod)
	h.RefreshAllCaches(t)
	if h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "schain-u1")).NoBid {
		t.Fatal("baseline (schain off): expected a winning bid for the basic world")
	}

	// Strict: the missing SupplyChain must now be rejected before fan-out — and
	// crucially the no-bid must carry NBR=501, so it's provably the schain gate
	// and not merely absent demand. This is the "tell a true no-bid from an
	// enforcement block" assertion.
	h.SetConfigForPod(t, "exchange.schain_enforcement", "strict", pod)
	h.RefreshAllCaches(t)
	got := h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "schain-u2"))
	if !got.NoBid {
		t.Error("strict schain: expected NoBid for a request carrying no SupplyChain")
	}
	if got.NBR != openrtb.NBRSChainInvalid {
		t.Errorf("strict schain: NBR = %d (%q), want %d (schain_invalid) — a block must be distinguishable from a genuine no-bid",
			got.NBR, got.NBRReason, openrtb.NBRSChainInvalid)
	}

	// Back to warn (the local default): the same request wins again — enforcement
	// is genuinely live-toggled, not a one-way ratchet.
	h.SetConfigForPod(t, "exchange.schain_enforcement", "warn", pod)
	h.RefreshAllCaches(t)
	if h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "schain-u3")).NoBid {
		t.Error("warn schain: expected a winning bid (missing schain logged, not rejected)")
	}
}
