//go:build e2e

// Buyer-supplied OpenRTB notice URLs end-to-end (§4.4): the exchange fires
// the BUYER's nurl on win and lurl on loss with the auction macros
// substituted (never literal), and the buyer's burl fires at the BILLABLE
// moment — when the impression books in the billing engine — not at auction
// time. Legacy fixed-endpoint notices remain the fallback for bids that
// carry no notice URLs.
package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// noticeFake builds a FakeDSP whose bids carry buyer-supplied nurl/lurl/burl
// pointing back at the fake itself, with the standard §4.4 macros.
func noticeFake(t *testing.T, h *harness.Harness, price float64, seat string) *harness.FakeDSP {
	t.Helper()
	var fake *harness.FakeDSP
	fake = harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode: harness.FakeDSPBidder, BidPrice: price, Seat: seat,
		BidMutate: func(bid *openrtb.BidObj, req openrtb.BidRequest) {
			base := fake.URL
			q := "bid_id=${AUCTION_BID_ID}&campaign_id=" + bid.CID + "&placement_id=${AUCTION_IMP_ID}"
			bid.NURL = base + routes.OpenRTBWin + "?" + q + "&price=${AUCTION_PRICE}&clear_price=${AUCTION_MIN_TO_WIN}"
			bid.LURL = base + routes.OpenRTBLoss + "?" + q + "&reason=${AUCTION_LOSS}&clearing_price=${AUCTION_PRICE}"
			bid.BURL = base + routes.OpenRTBBilling + "?" + q + "&price=${AUCTION_PRICE}"
			bid.AdM = `<div data-buyer-brand="NOTICEFAKE">notice test</div>`
			bid.W, bid.H = 300, 250
			bid.ADomain = []string{seat + ".example"}
		},
	})
	return fake
}

// TestBuyerNoticeURLs: winner's nurl and loser's lurl fire with substituted
// macros; the winner's burl does NOT fire at auction time, then fires once
// the impression books.
func TestBuyerNoticeURLs(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "notices")

	winner := noticeFake(t, h, 40.0, "notice-winner")
	loser := noticeFake(t, h, 5.0, "notice-loser")
	h.SetConfigForPod(t, "exchange.dsp_endpoints", winner.URL+","+loser.URL, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	// Serve (display) so the impression URL exists for the burl leg below.
	var res harness.SSPServeResult
	harness.WaitFor(t, 20*time.Second, "notice winner fills", func() bool {
		res = h.ServeViaSSP(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: fmt.Sprintf("notices-user-%d", time.Now().UnixNano()), IP: "203.0.113.30",
		})
		return !res.NoBid && strings.Contains(res.HTML, "NOTICEFAKE")
	})

	// --- nurl: fired to the WINNER with the clearing price substituted.
	harness.WaitFor(t, 15*time.Second, "buyer nurl fired", func() bool {
		return len(winner.WinCalls()) >= 1
	})
	win := winner.WinCalls()[0]
	if win.Price != "40.0000" {
		t.Errorf("nurl ${AUCTION_PRICE} not substituted with the clearing price: %+v", win)
	}
	if win.BidID != "fake-bid" {
		t.Errorf("nurl ${AUCTION_BID_ID} not substituted: %+v", win)
	}
	if strings.Contains(win.Price+win.BidID+win.CampaignID+win.PlacementID, "${") {
		t.Fatalf("literal macro reached the buyer's win notice: %+v", win)
	}

	// --- lurl: fired to the LOSER with reason + winning price substituted.
	harness.WaitFor(t, 15*time.Second, "buyer lurl fired", func() bool {
		return len(loser.LossCalls()) >= 1
	})
	loss := loser.LossCalls()[0]
	if loss.Reason != "102" { // LossOutbid
		t.Errorf("lurl ${AUCTION_LOSS} not substituted with the outbid reason: %+v", loss)
	}
	if loss.ClearingPrice != "40.0000" {
		t.Errorf("lurl ${AUCTION_PRICE} not substituted with the winning price: %+v", loss)
	}

	// --- burl: NOT at auction time — the billable moment is the impression.
	if n := len(winner.BillingCalls()); n != 0 {
		t.Fatalf("burl fired at auction time (%d calls) — must wait for the impression", n)
	}
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

	harness.WaitFor(t, 20*time.Second, "burl fired at the billable moment", func() bool {
		return len(winner.BillingCalls()) >= 1
	})
	bill := winner.BillingCalls()[0]
	if bill.Price != "40.0000" {
		t.Errorf("burl ${AUCTION_PRICE} not substituted: %+v", bill)
	}
	// Exactly-once: a second impression fire for the same trace must not
	// re-fire the claimed notice.
	resp2, err := h.HTTP.Do(req)
	if err == nil {
		resp2.Body.Close()
	}
	time.Sleep(2 * time.Second)
	if n := len(winner.BillingCalls()); n != 1 {
		t.Errorf("burl must fire exactly once per trace, got %d", n)
	}
}

// TestLegacyNoticeFallback (negative): a bid WITHOUT notice URLs still gets
// the legacy fixed-endpoint win notice — older/minimal bidders keep working.
func TestLegacyNoticeFallback(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "notices-legacy")

	fake := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode: harness.FakeDSPBidder, BidPrice: 40.0, Seat: "legacy-seat",
	}) // no BidMutate → no nurl/lurl/burl
	h.SetConfigForPod(t, "exchange.dsp_endpoints", fake.URL, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	harness.WaitFor(t, 20*time.Second, "legacy win notice arrives", func() bool {
		h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile",
			fmt.Sprintf("legacy-user-%d", time.Now().UnixNano()))
		return len(fake.WinCalls()) >= 1
	})
	if win := fake.WinCalls()[0]; win.Price == "" || strings.Contains(win.Price, "${") {
		t.Errorf("legacy win notice malformed: %+v", win)
	}
}
