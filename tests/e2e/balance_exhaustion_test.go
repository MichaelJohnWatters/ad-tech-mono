//go:build e2e

// Balance exhaustion — the prepay posture end-to-end: a small topup funds a
// few wins, the DSP's balance gate then stops bidding account-wide, and a
// fresh topup resumes it. Small overshoot (an in-flight win past zero) is
// the designed tolerance.
package e2e

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestBalanceExhaustionStopsBidding(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	// Reset so this test's world is the only demand — with other funded
	// campaigns around, the account-level no-bid would be masked by their
	// bids filling the auction.
	h.Reset(t)
	h.ResetBillingLedger(t)

	// Publisher side via API (site + placement to auction against).
	uniq := fmt.Sprintf("exhaust-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Exhaust Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, "POST", "/v1/api/publishers", `{"name":"Exhaust Site","domain":"`+uniq+`.test"}`)
	pl := h.APIJSON(t, pub, "POST", "/v1/api/placements",
		fmt.Sprintf(`{"publisher_id":%q,"name":"Exhaust MPU","format":"display","width":300,"height":250,"floor_price":0.5}`, site["id"]))
	placementID := pl["id"].(string)

	// Advertiser with a TINY wallet. base_bid 2.50 is a CPM, so each win draws
	// down 2.50/1000 = $0.0025. A $0.006 wallet → 2 clean wins ($0.005), a
	// possible overshoot 3rd, then the gate must flip.
	adv := h.Signup(t, "Tiny Wallet", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	h.APIJSON(t, adv, "POST", "/v1/api/campaigns", `{"name":"Tiny Wallet Campaign","base_bid":2.5,"daily_budget":100}`)
	h.APIJSON(t, adv, "POST", "/v1/api/billing/topup", `{"amount":0.006,"idempotency_key":"`+uniq+`-seed"}`)
	h.RefreshAllCaches(t)

	hasBid := func(res harness.AuctionResult) bool {
		var br struct {
			SeatBid []struct {
				Bid []struct {
					Price float64 `json:"price"`
				} `json:"bid"`
			} `json:"seatbid"`
		}
		_ = json.Unmarshal(res.BidResponse, &br)
		return len(br.SeatBid) > 0 && len(br.SeatBid[0].Bid) > 0
	}

	// Auctions win while funds last, then the gate flips. The DSP bid loop reads
	// budget/balance from a warm in-process mirror refreshed every
	// dsp.bid_cache_refresh_interval (1s) AFTER the win's drawdown settles async —
	// so the overshoot past zero is however many auctions clear in that
	// settle+refresh window, NOT a fixed 1. Under full-suite load that's a
	// handful; it is the DESIGNED tolerance of a polled prepay gate (not a bug).
	// So assert only that funds bought the 2 clean wins AND that the gate DOES
	// eventually flip — a broken gate never flips, which the loop still catches.
	// A short beat between auctions lets the drawdown+refresh land within the loop.
	wins, noBidSeen := 0, false
	for i := 0; i < 25 && !noBidSeen; i++ {
		if hasBid(h.RunAuction(t, placementID, "GBR", "mobile", fmt.Sprintf("exhaust-user-%d", i))) {
			wins++
			time.Sleep(400 * time.Millisecond) // let the async drawdown + 1s mirror refresh catch up
			continue
		}
		noBidSeen = true
	}
	if !noBidSeen {
		t.Fatalf("gate never flipped: %d straight wins on a $0.006 wallet at $0.0025/win", wins)
	}
	// Lower bound: the wallet funds 2 clean wins. Upper bound: the polled gate
	// overshoots by however many auctions clear in the settle+refresh window
	// (~1-2s → a handful at 400ms/auction), but NOT unboundedly — keep a generous
	// ceiling so a gate that flips FAR too late (reads a stale mirror, lets the
	// account run deep negative) still fails. A healthy gate flips at 2-5 wins; 10
	// is ~4× the wallet, well clear of the tolerance yet catches gross overspend —
	// this ceiling is the money-invariant guard, don't drop it for flake-proofing.
	if wins < 2 || wins > 10 {
		t.Errorf("wins before exhaustion = %d, want 2-10 ($0.006 wallet funds 2 at $0.0025; >10 = gate flipped far too late, gross overspend)", wins)
	}
	t.Logf("exhaustion: %d wins on a $0.006 wallet (2 funded + polled-gate overshoot), then no-bid", wins)

	// A refill topup publishes the balances invalidate — bidding resumes
	// within NATS RTT (poll a few auctions to absorb propagation).
	h.APIJSON(t, adv, "POST", "/v1/api/billing/topup", `{"amount":50,"idempotency_key":"`+uniq+`-refill"}`)
	resumed := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if hasBid(h.RunAuction(t, placementID, "GBR", "mobile", "post-refill-user")) {
			resumed = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !resumed {
		t.Fatalf("bidding never resumed after the refill topup")
	}
	t.Log("refill topup resumed bidding — money loop closed both directions")
}
