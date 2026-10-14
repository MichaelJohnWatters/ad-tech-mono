//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestExchangeHonoursSSPTMax — OpenRTB tmax is enforced end-to-end:
// SSP stamps ssp.tmax → exchange fan-out deadline = min(bid_timeout,
// tmax − exchange.tmax_margin). Asserted on auction OUTCOME, not wall-clock
// (timing asserts flake on a loaded stack): every DSP is stalled 150ms via
// X-Dev-Slow-DSPs.
//
//   - control: default ssp.tmax (525ms → 500ms deadline) — the 150ms bids
//     land in time and the auction has a winner.
//   - enforced: ssp.tmax=60ms (→ 35ms deadline) — every leg is cut, so the
//     auction must come back no-bid. Before tmax support the exchange
//     ignored tmax (the SSP's hard-coded 100 was decorative) and this case
//     still found a winner.
func TestExchangeHonoursSSPTMax(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	h.WithDeterministicCompetitors(t)
	h.RefreshAllCaches(t)
	t.Cleanup(func() { h.SetConfigForPod(t, "ssp.tmax", "525ms", harness.PodSSP) })

	const allSlow = "0,1,2,3,4,5,6,7"

	h.SetConfigForPod(t, "ssp.tmax", "525ms", harness.PodSSP)
	ctl := h.ExtractWinner(t, h.RunAuctionWithSlowDSPs(t, "pl-news-mpu", "GBR", "mobile", "tmax-ctl-user", allSlow))
	if ctl.NoBid {
		t.Fatal("control: 150ms-stalled DSPs should beat the default 500ms deadline and produce a winner")
	}

	h.SetConfigForPod(t, "ssp.tmax", "60ms", harness.PodSSP)
	got := h.ExtractWinner(t, h.RunAuctionWithSlowDSPs(t, "pl-news-mpu", "GBR", "mobile", "tmax-enf-user", allSlow))
	if !got.NoBid {
		t.Fatalf("tmax=60ms (35ms fan-out deadline) must cut every 150ms-stalled DSP; got a winner at %v", got.Price)
	}
}

// TestExchangePerChannelBidTimeout — exchange.bid_timeout_by_channel overrides
// the global bid_timeout for the named channel only. pl-news-mpu is display:
// with every DSP stalled 150ms, display=35ms must no-bid while an override for
// a DIFFERENT channel (video) leaves display on the global 500ms (winner).
func TestExchangePerChannelBidTimeout(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	h.WithDeterministicCompetitors(t)
	h.RefreshAllCaches(t)
	t.Cleanup(func() { h.SetConfigForPod(t, "exchange.bid_timeout_by_channel", "", harness.PodExchange) })

	const allSlow = "0,1,2,3,4,5,6,7"

	h.SetConfigForPod(t, "exchange.bid_timeout_by_channel", "video=35ms", harness.PodExchange)
	other := h.ExtractWinner(t, h.RunAuctionWithSlowDSPs(t, "pl-news-mpu", "GBR", "mobile", "chto-other-user", allSlow))
	if other.NoBid {
		t.Fatal("a video-only override must not tighten a display auction")
	}

	h.SetConfigForPod(t, "exchange.bid_timeout_by_channel", "display=35ms", harness.PodExchange)
	got := h.ExtractWinner(t, h.RunAuctionWithSlowDSPs(t, "pl-news-mpu", "GBR", "mobile", "chto-display-user", allSlow))
	if !got.NoBid {
		t.Fatalf("display=35ms must cut every 150ms-stalled DSP; got a winner at %v", got.Price)
	}
}
