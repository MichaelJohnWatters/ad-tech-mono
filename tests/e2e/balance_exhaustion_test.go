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

	// Advertiser with a TINY wallet: 6.00 at 2.50/bid → 2 clean wins, a
	// possible overshoot 3rd, then the gate must flip.
	adv := h.Signup(t, "Tiny Wallet", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	h.APIJSON(t, adv, "POST", "/v1/api/campaigns", `{"name":"Tiny Wallet Campaign","base_bid":2.5,"daily_budget":100}`)
	h.APIJSON(t, adv, "POST", "/v1/api/billing/topup", `{"amount":6,"idempotency_key":"`+uniq+`-seed"}`)
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

	// Auctions win while funds last; the gate flips within the wallet's
	// arithmetic (2 wins + at most 1 overshoot on 6.00 at 2.50).
	wins, noBidSeen := 0, false
	for i := 0; i < 10 && !noBidSeen; i++ {
		if hasBid(h.RunAuction(t, placementID, "GBR", "mobile", fmt.Sprintf("exhaust-user-%d", i))) {
			wins++
			continue
		}
		noBidSeen = true
	}
	if !noBidSeen {
		t.Fatalf("gate never flipped: %d straight wins on a 6.00 wallet at 2.50/win", wins)
	}
	if wins < 2 || wins > 3 {
		t.Errorf("wins before exhaustion = %d, want 2-3 (6.00 wallet, 2.50 bids, ≤1 overshoot)", wins)
	}
	t.Logf("exhaustion: %d wins on a 6.00 wallet, then no-bid", wins)

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
