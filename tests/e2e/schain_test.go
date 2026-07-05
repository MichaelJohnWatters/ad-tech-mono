//go:build e2e

// SupplyChain (schain) transparency tests.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestSChainStrictRejectsMissing — with exchange.schain_enforcement=strict, an
// auction whose bid request carries no valid schain must be rejected (NoBid)
// before fan-out; with enforcement back to the "warn" default the same request
// proceeds. Exercises the exchange gate (cmd/exchange/schain.go) end-to-end.
//
// Pending two pieces of harness support:
//  1. ssp.seller_domain is a *static*-tier config key (read once at SSP boot),
//     so it can't be toggled live via SetConfigForPod — the local Tilt overlay
//     must set it for the SSP to originate a schain at all. Until then, whether
//     a served auction contains a schain depends on deploy config, so the
//     strict/allow outcome isn't deterministic from the test's point of view.
//  2. The auction *response* doesn't echo the outbound bid request, so we can't
//     directly assert source.ext.schain.nodes[0] == {asi, sid}. A helper that
//     captures the SSP→exchange request (or an exchange debug echo) would let us
//     assert the chain contents, not just the pass/reject behaviour.
//
// The construction + validation logic is unit-tested: pkg/openrtb (ValidateSChain,
// SChainOf) and cmd/ssp (originSChain). The harness now carries privacy signals
// (AuctionParams.GDPR/Consent/USPrivacy/…) and can write exchange.schain_enforcement
// live via SetConfigForPod("exchange-0", …), so this test is ready to flip once
// (1) lands.
func TestSChainStrictRejectsMissing(t *testing.T) {
	t.Skip("pending Tilt overlay setting ssp.seller_domain (static tier) + a harness capture of the SSP outbound bid request; see comment. Gate + construction logic covered by unit tests in pkg/openrtb and cmd/ssp.")

	// Sketch of the intended assertion once the blockers clear:
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "schain")
	t.Cleanup(func() { h.SetConfigForPod(t, "exchange.schain_enforcement", "warn", "exchange-0") })

	h.SetConfigForPod(t, "exchange.schain_enforcement", "strict", "exchange-0")
	h.RefreshAllCaches(t)
	win := h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "schain-user"))
	_ = win // assert win.NoBid when no schain present, or a winner when the SSP originates one
}
