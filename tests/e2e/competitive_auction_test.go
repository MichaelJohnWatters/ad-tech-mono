//go:build e2e

// Competitive auction suite — verifies the core "many possible winners,
// the right one wins" behaviour at the exchange. Until now every e2e test
// had exactly one matching campaign, so the auction never actually had
// to choose. These tests stand up two-or-more bidders per scenario and
// assert on the outcome.
//
// FULL SCOPE (6 phases, 19 tests). Each row = one test in this file.
//
// PHASE 1 — core auction proof
//
//	A1  HigherBaseBidWinsWithinDSP                       within-DSP selection
//	B1  CrossDSPHighestBidWins                           exchange-side selection
//	C1  FloorCutsLowBidsHighestAboveFloorWins            floor enforcement
//	B2  OneDSPNoBidsOthersWin                            partial fan-out
//
// PHASE 2 — robustness
//
//	B4  SlowDSPCutOffByBidTimeout                        timeout protection
//	B6  ErrorFromOneDSPDoesNotKillAuction                error tolerance
//	A3  BudgetExhaustedCampaignExcludedSiblingBids       per-campaign budget
//	A4  PausedCampaignExcludedSiblingBids                status filter
//	D1  PGDealPreemptsHigherOpenBid                      deal preempt
//
// PHASE 3 — fan-out hygiene
//
//	B5  AllDSPsTimeoutReturnsNoBid                       no partial deadlock
//	B7  SmartRouterPreFiltersAlwaysNoBidDSP              router learning
//	E1  LosersReceiveLossNotifications                   notification fan-out
//
// PHASE 4 — publisher-adserver vs programmatic
//
//	F1  SponsorshipPreemptsHigherProgrammaticBid         tier ordering
//	F4  OutOfFlightDirectSkippedProgrammaticWins         flight window
//	F5  DirectLineItemPlacementAllowlistRespected        placement match
//
// PHASE 5 — deals competitive
//
//	D2  PreferredDealRaisesFloorOpenBidStillWins         non-preempt deal
//	D3  PMPAllowlistedAtDealPriceVsNonListedOpen         PMP eligibility
//
// PHASE 6 — Prebid competitive
//
//	G1  PrebidMultiImpRequestPerImpAuction               multi-imp handling
//	G2  PrebidInboundVsInternalDSPsHighestWins           Prebid + fan-out
package e2e

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// deriveID matches pkg/idgen.Derive — kept as a local alias so each test
// site reads as "give me the UUID for this external key" without an extra
// import line.
func deriveID(kind, key string) string { return idgen.Derive(kind, key) }

// ─────────────────────────────────────────────────────────────────────────
// PHASE 1 — core auction proof
// ─────────────────────────────────────────────────────────────────────────

// TestCompetitiveA1_HigherBaseBidWinsWithinDSP — two campaigns under the
// same advertiser with identical targeting but different base_bids. The DSP
// must pick the higher one to bid; assert the winning bid reflects the
// higher base_bid (within shading tolerance, which we set wide enough to
// not flake).
func TestCompetitiveA1_HigherBaseBidWinsWithinDSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-a1")

	// BasicWorld already has a campaign at 3.50. Add a sibling at 7.00 with
	// the same targeting under the same advertiser account.
	hi := h.CreateCampaign(t, w.AdvAcc, w.IO,
		"comp-a1-li-hi",
		7.00, // base_bid — should win over the basic world's 3.50
		500,
		"comp-a1-cr-hi",
		"adv-comp-a1.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}},
	)
	_ = hi
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-a1-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected winning bid; got no_bid")
	}
	// Price should be above the BasicWorld 3.50 ceiling — proves the
	// higher-bid sibling was selected (after shading, exact price is
	// approximate but should land in [3.5, 7.0]).
	if win.Price < 3.5 || win.Price > 7.5 {
		t.Errorf("price %v outside expected range [3.5, 7.0] — higher sibling not selected?", win.Price)
	}
	// Winning campaign id should be the higher-bid one.
	if win.CampaignID != hi.ID {
		t.Errorf("winning CampaignID = %q; want hi-bid sibling %q (BasicWorld campaign is %q)",
			win.CampaignID, hi.ID, w.Campaign.ID)
	}
}

// TestCompetitiveB1_CrossDSPHighestBidWins — re-seeds the standard profile
// so each of the 3 DSPs has live campaigns, zeroes the competitor DSPs'
// noise and random-no-bid knobs, and runs an auction. With realistic seeded
// campaigns and zero noise, comp1's MegaStore campaign (base 3.50 × 1.30
// mobile modifier ≈ 4.55) should beat internal's Acme Shoes UK Mobile
// (base 2.50, no modifier).
//
// Asserts the winner came from comp1 (clearing price > 4.0 — a value
// internal can't reach on its base_bid). If this fails the cross-DSP
// fan-out + price-comparison auction core is broken.
func TestCompetitiveB1_CrossDSPHighestBidWins(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	// Load-bearing: empirically confirmed (10-run flake test, 4+ failures
	// observed) — without zeroing comp1's ±30% noise + 20% no-bid rate,
	// comp1 either no-bids or undercuts the 4.0 assertion threshold.
	h.WithDeterministicCompetitors(t)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, "pl-news-mpu", "GBR", "mobile", "comp-b1-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected a winning bid (3 DSPs deterministic, GBR+mobile should match multiple)")
	}
	if win.Price < 4.0 {
		t.Errorf("clearing price %v < 4.0 — competitor1 (MegaStore @ ~4.55) didn't win, internal (~2.50) did?", win.Price)
	}
}

// TestCompetitiveC1_FloorCutsLowBidsHighestAboveFloorWins — three competing
// campaigns at 0.50, 2.00, 4.00 against a placement with floor=1.50. The
// 0.50 bid must be dropped; the 4.00 wins (not the 2.00). Proves the
// effective floor is applied before the winner-pick, not after.
func TestCompetitiveC1_FloorCutsLowBidsHighestAboveFloorWins(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-c1")

	// BasicWorld placement floor is 1.00. Raise to 1.50 to cut the 0.50 bid.
	h.WithTenant(t, w.PubAcc.ID, func(tx *sql.Tx) {
		if _, err := tx.Exec(`UPDATE placements SET floor_price = 1.50 WHERE id = $1`, w.Placement.ID); err != nil {
			t.Fatalf("set placement floor: %v", err)
		}
	})

	// BasicWorld campaign already bids 3.50. We need 0.50 and 4.00 too.
	// Lower the BasicWorld one to 0.50 to be the below-floor case.
	h.SetCampaignBaseBid(t, w.Campaign, 0.50)
	mid := h.CreateCampaign(t, w.AdvAcc, w.IO,
		"comp-c1-li-mid", 2.00, 500,
		"comp-c1-cr-mid", "adv-comp-c1-mid.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}},
	)
	hi := h.CreateCampaign(t, w.AdvAcc, w.IO,
		"comp-c1-li-hi", 4.00, 500,
		"comp-c1-cr-hi", "adv-comp-c1-hi.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}},
	)
	_ = mid
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-c1-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected winning bid (4.00 > floor 1.50)")
	}
	if win.CampaignID != hi.ID {
		t.Errorf("winner CampaignID = %q; want hi-bid %q", win.CampaignID, hi.ID)
	}
	if win.Price < 1.5 {
		t.Errorf("clearing price %v < floor 1.50 — floor not enforced", win.Price)
	}
}

// TestCompetitiveB2_OneDSPNoBidsOthersWin — force competitor1 to always
// no-bid; competitor2 + internal still bid normally. Auction must resolve
// from the remaining DSPs rather than fail. Proves partial fan-out
// doesn't break the response path.
func TestCompetitiveB2_OneDSPNoBidsOthersWin(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	// No WithDeterministicCompetitors: MakeDSPAlwaysNoBid pins comp1
	// explicitly, and internal-DSP is profile=internal (0% noise / 0%
	// no-bid by default) so a winner is guaranteed even if comp2 noises
	// its bid down.
	h.MakeDSPAlwaysNoBid(t, harness.PodDSPCompetitor1)
	h.RefreshAllCaches(t)

	// The no-bid config flip propagates to comp1 via NATS invalidate +
	// config poll — retry briefly instead of racing the very next auction
	// against it.
	var win harness.BidResponseWinner
	harness.WaitFor(t, 35*time.Second, "auction resolves with comp1 out", func() bool {
		res := h.RunAuction(t, "pl-news-mpu", "GBR", "mobile", "comp-b2-user")
		win = h.ExtractWinner(t, res)
		return !win.NoBid
	})
	if win.Price <= 0 {
		t.Errorf("clearing price %v; want > 0", win.Price)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PHASE 2 — robustness
// ─────────────────────────────────────────────────────────────────────────

// TestCompetitiveB4_SlowDSPCutOffByBidTimeout — set bid_timeout=200ms and
// use X-Dev-Slow-DSPs to make DSP index 0 (internal) deliberately slow.
// Comp1 + comp2 should still bid in time, auction resolves with one of
// them. Proves the deadline-context cuts off slow DSPs without holding
// up the whole auction.
func TestCompetitiveB4_SlowDSPCutOffByBidTimeout(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	h.WithDeterministicCompetitors(t)
	h.SetExchangeBidTimeout(t, "200ms")
	h.RefreshAllCaches(t)

	// "0" is the internal DSP's index in exchange.dsp_endpoints (the comma-
	// separated list starts with internal). Slowing it forces a 200ms+
	// delay before it would respond, longer than bid_timeout.
	res := h.RunAuctionWithSlowDSPs(t, "pl-news-mpu", "GBR", "mobile", "comp-b4-user", "0")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected one of the non-slow DSPs to win")
	}
	if win.Price <= 0 {
		t.Errorf("clearing price %v; want > 0", win.Price)
	}
}

// TestCompetitiveB6_ErrorFromOneDSPDoesNotKillAuction — swaps a broken
// fake DSP into the front of exchange.dsp_endpoints. The exchange must
// keep going after the fake returns 500, falling through to the real
// internal DSP for a winning bid.
//
// Proves: a misbehaving DSP doesn't take down the auction. Before this
// test, that was an assumption — we'd never seen an exchange respond
// after a real 5xx in the fan-out.
func TestCompetitiveB6_ErrorFromOneDSPDoesNotKillAuction(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-b6")

	broken := harness.NewFakeDSP(t, harness.FakeDSPOpts{Mode: harness.FakeDSPBroken})

	// Put the broken fake first so the exchange definitely tries it; keep
	// the real internal DSP in the list so SOMETHING bids. Use Cluster*
	// DNS for the real DSP because the exchange pod reads this config
	// value and dials it from inside the cluster — localhost would be
	// the exchange pod's own loopback. Restore the default endpoint list
	// on cleanup so later tests aren't poisoned.
	h.SetConfigForPod(t, "exchange.dsp_endpoints",
		broken.URL+","+h.URLs.ClusterDSP, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-b6-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected auction to resolve despite broken DSP; got no_bid")
	}
	if broken.BidCalls() < 1 {
		t.Errorf("broken fake DSP never called — exchange.dsp_endpoints swap didn't take effect (BidCalls=%d)", broken.BidCalls())
	}
}

// TestCompetitiveA3_BudgetExhaustedCampaignExcludedSiblingBids — campaign A
// has a tiny daily_budget (1.00) that gets consumed by one auction. A
// sibling campaign B with the same targeting and a larger budget keeps
// bidding. After exhaustion, the next auction's winner must be B, not A.
func TestCompetitiveA3_BudgetExhaustedCampaignExcludedSiblingBids(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-a3")

	// BasicWorld's campaign starts at 3.50 / 500 daily budget. Lower its
	// budget below one impression's realized cost (a 3.50 CPM win books
	// 3.50/1000 = 0.0035 against the budget post money-precision) and add
	// a sibling at 2.00 with healthy budget.
	h.SetCampaignBaseBid(t, w.Campaign, 3.50)
	h.SetCampaignDailyBudget(t, w.Campaign, 0.001)
	sibling := h.CreateCampaign(t, w.AdvAcc, w.IO,
		"comp-a3-li-sibling", 2.00, 500,
		"comp-a3-cr-sibling", "adv-comp-a3.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}},
	)
	h.RefreshAllCaches(t)

	// Fire one auction to win + exhaust the 1.00 budget on BasicWorld
	// campaign. Then a second auction should be won by the sibling.
	first := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-a3-user-1")
	if h.ExtractWinner(t, first).NoBid {
		t.Fatal("first auction should have a winner")
	}

	// The win's budget drawdown settles async, then the DSP's budget tracker
	// picks it up on its next warm-mirror refresh (dsp.bid_cache_refresh_interval,
	// 1s) — so the exhausted campaign stays eligible for a beat. A fixed 150ms
	// sleep raced that 1s refresh (structurally too short, worse under load), so
	// POLL the auction until budget exclusion has propagated and the sibling wins.
	// The sibling always has budget, so the winner flips to it once A is excluded.
	var last harness.BidResponseWinner
	harness.WaitFor(t, 15*time.Second, "budget-exhausted campaign excluded → sibling wins", func() bool {
		second := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-a3-user-2")
		last = h.ExtractWinner(t, second)
		return !last.NoBid && last.CampaignID == sibling.ID
	})
	t.Logf("budget exclusion propagated: second auction won by sibling %q", last.CampaignID)
}

// TestCompetitiveA4_PausedCampaignExcludedSiblingBids — pause the higher-
// bid campaign; the lower-bid sibling must win the next auction. Proves
// status='paused' actually pulls the campaign from the bidding pool, not
// just out of the UI.
func TestCompetitiveA4_PausedCampaignExcludedSiblingBids(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-a4")

	// BasicWorld @ 3.50. Add sibling @ 5.00 (would normally win), then pause it.
	hi := h.CreateCampaign(t, w.AdvAcc, w.IO,
		"comp-a4-li-hi", 5.00, 500,
		"comp-a4-cr-hi", "adv-comp-a4-hi.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}},
	)
	h.PauseCampaign(t, hi)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-a4-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected BasicWorld campaign to win; got no_bid")
	}
	if win.CampaignID != w.Campaign.ID {
		t.Errorf("winner = %q; want BasicWorld campaign %q (hi-bid was paused)",
			win.CampaignID, w.Campaign.ID)
	}
}

// TestCompetitiveD1_PGDealPreemptsHigherOpenBid — set up a PG deal at 5.00
// for our test advertiser, alongside a competitor DSP that bids higher
// (~7) on the same placement. PG must preempt regardless of price.
// Asserts DealID is set on the winning bid (proves PG path ran, not just
// "highest bid won").
func TestCompetitiveD1_PGDealPreemptsHigherOpenBid(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-d1")
	// No WithDeterministicCompetitors: BuildBasicWorld truncates campaign
	// state so comp1/comp2 have nothing to bid, and PG preempt is decided
	// at the exchange independent of any competitor's bid.

	// PG deal: test advertiser on test placement, deal price 5.00.
	dealID := h.CreateDeal(t, w.Publisher, "comp-d1-deal", "pg", 5.00,
		[]string{w.AdvAcc.ID}, []string{w.Placement.ID})
	_ = dealID

	// Set BasicWorld campaign bid HIGHER than 5.00 so it's eligible at PG
	// price. Without PG, this same bid would win an open auction anyway —
	// the PG-path assertion is on DealID being non-empty on the winner.
	h.SetCampaignBaseBid(t, w.Campaign, 6.00)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-d1-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("PG deal should preempt and produce a winner")
	}
	if win.DealID == "" {
		t.Errorf("winner DealID empty — PG preempt path not exercised (winner=%+v)", win)
	}
	// Clearing price should be the deal price (5.00), not the bid (6.00).
	if win.Price > 5.5 {
		t.Errorf("clearing price %v > deal price 5.00 — PG should clear AT deal price", win.Price)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PHASE 3 — fan-out hygiene
// ─────────────────────────────────────────────────────────────────────────

// TestCompetitiveB5_AllDSPsTimeoutReturnsNoBid — force every DSP to be slow
// with a tight bid_timeout. Auction must complete (no hang) and return
// nobid. Proves the early-finish path on ctx.Done works.
func TestCompetitiveB5_AllDSPsTimeoutReturnsNoBid(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	// No WithDeterministicCompetitors: every DSP is stalled by the
	// X-Dev-Slow-DSPs header; noise on bids that never return is moot.
	h.SetExchangeBidTimeout(t, "50ms")
	h.RefreshAllCaches(t)

	// The 50ms bid_timeout propagates to the exchange via NATS invalidate
	// (instant) with the manager's 30s Postgres poll as fallback — so the
	// retry window covers a full poll cycle. An auction fired before the flip
	// lands still runs at the default timeout: it may nobid too (the slow DSPs
	// never answer), but slowly. The success condition is therefore nobid AND
	// fast completion — that pair only happens once the 50ms deadline is live,
	// and it IS the assertion (no hang, early-finish on ctx.Done works).
	harness.WaitFor(t, 35*time.Second, "exchange picks up the 50ms bid_timeout (all DSPs slow → fast nobid)", func() bool {
		start := time.Now()
		res := h.RunAuctionWithSlowDSPs(t, "pl-news-mpu", "GBR", "mobile", "comp-b5-user", "0,1,2")
		elapsed := time.Since(start)
		return h.ExtractWinner(t, res).NoBid && elapsed < 1*time.Second
	})
}

// TestCompetitiveB7_SmartRouterPreFiltersAlwaysNoBidDSP — train the smart
// router with >20 auctions where competitor1 always returns no_bid; then
// ask the router (via /v1/openrtb/routing?preview=true) which DSPs it
// would call next. Comp1 must be excluded — the router's "bid_rate < 5%
// after 20 calls → skip" rule should have fired.
//
// Proves: the router doesn't just *track* bad DSPs, it *acts on* the
// stats by filtering future fan-outs. Before this, only the recording
// path was tested (TestSmartRoutingTracksDSPStats).
func TestCompetitiveB7_SmartRouterPreFiltersAlwaysNoBidDSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	// No WithDeterministicCompetitors: MakeDSPAlwaysNoBid overrides comp1
	// explicitly (no_bid_rate=1.0), so comp1's natural noise is irrelevant.
	// Comp2's noise doesn't affect the comp1-only assertion.
	h.MakeDSPAlwaysNoBid(t, harness.PodDSPCompetitor1)
	h.RefreshAllCaches(t)

	// Reset ONCE (stamps the reseed window: dsp_calls since resetAt), then feed
	// no-bids until comp1 drops. comp1 is no_bid_rate=1.0, so every recorded call
	// is a no-bid. The skip needs the (display, comp1) bucket to cross
	// routing_min_calls (20) in the CLUSTER-GLOBAL aggregate, but the skip is
	// SELF-LIMITING: once a pod starts skipping comp1 it stops recording its
	// calls, which can freeze the aggregate JUST BELOW the threshold — a one-shot
	// "fire 30 then poll" can strand comp1 at e.g. 17 recorded calls forever
	// (below min_calls, so never skipped, yet nothing firing to push it over).
	// (The OLD approach re-RESET inside the loop, which moved the window start
	// forward faster than the 5s reseed could act — a different failure, also
	// removed.) The escape: at the sub-threshold frozen state neither pod skips,
	// so fresh auctions DO record + climb the aggregate. Fire a small batch each
	// poll iteration until comp1 crosses over and the reseed converges the pods.
	h.ResetSmartRouter(t)
	h.FireNAuctions(t, 24, "pl-news-mpu", "GBR", "mobile")
	var preview harness.RouterPreview
	comp1Dropped := func() bool {
		preview = h.SmartRouterPreview(t)
		for _, ep := range preview.Selected {
			// In pod mode, the exchange holds cluster-DNS endpoints
			// (http://dsp-competitor1:8089), so compare against Cluster*.
			if ep == h.URLs.ClusterDSPComp1 {
				return false // comp1 still selected — reseed hasn't converged yet
			}
		}
		return true
	}
	harness.WaitFor(t, 90*time.Second, "router learns to skip always-no-bid comp1", func() bool {
		if comp1Dropped() {
			return true
		}
		h.FireNAuctions(t, 6, "pl-news-mpu", "GBR", "mobile")
		return comp1Dropped()
	})
	// Sanity: at least one DSP should still be selected (internal at minimum).
	if len(preview.Selected) == 0 {
		t.Fatalf("router excluded EVERY DSP; preview=%+v", preview)
	}
}

// TestCompetitiveE1_LosersReceiveLossNotifications — swaps a low-bidding
// fake DSP into exchange.dsp_endpoints alongside the real internal DSP.
// BasicWorld's campaign bids 3.50; we set the fake to bid 0.50 so the
// real DSP wins. After the auction, the exchange must send a /loss call
// to the fake with the clearing price (proves the win/loss fanout
// actually runs to completion, not just builds the notification).
//
// Proves: losers get notified. Before this, the loss path was wired but
// never exercised end-to-end in a test.
func TestCompetitiveE1_LosersReceiveLossNotifications(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-e1")

	loser := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode:     harness.FakeDSPBidder,
		BidPrice: 0.50, // below the BasicWorld 3.50 → fake loses
		Seat:     "fake-loser-seat",
	})

	h.SetConfigForPod(t, "exchange.dsp_endpoints",
		loser.URL+","+h.URLs.ClusterDSP, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-e1-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("real DSP should have won at 3.50 over fake's 0.50; got no_bid")
	}

	// Win/loss notifications fan out asynchronously after the auction response
	// (exchange → HTTP callback to the loser). Poll rather than a fixed sleep, and
	// give it a generous window: the fanout runs in a goroutine after the auction
	// reply, so under full-suite load or on a cold VM it can take several seconds
	// to land. The old 2s deadline raced that (a flaky "never received /loss"),
	// while the callback DOES arrive — the poll returns the instant it does, so the
	// wider window only costs time on a genuine drop.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && len(loser.LossCalls()) == 0 {
		time.Sleep(100 * time.Millisecond)
	}

	loss := loser.LossCalls()
	if len(loss) == 0 {
		t.Fatalf("fake DSP never received a /loss callback (BidCalls=%d, WinCalls=%d)",
			loser.BidCalls(), len(loser.WinCalls()))
	}
	if loss[0].ClearingPrice == "" {
		t.Errorf("loss notification missing clearing_price (raw=%+v)", loss[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PHASE 4 — publisher-adserver vs programmatic
// ─────────────────────────────────────────────────────────────────────────

// TestCompetitiveF1_SponsorshipPreemptsHigherProgrammaticBid — set up a
// publisher direct sponsorship at low CPM (5.00) alongside a programmatic
// campaign that would bid 50.00 (well above competitor noise range).
// Through publisher-adserver, sponsorship must still win — proves the
// arbitration ladder is "contract trumps revenue", not price-based.
func TestCompetitiveF1_SponsorshipPreemptsHigherProgrammaticBid(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-f1")

	// Programmatic side: bid 50 (well above competitor noise).
	h.SetCampaignBaseBid(t, w.Campaign, harness.OverbidCompetitors)

	// Direct sponsorship side: priced at only 5.00.
	flightStart := time.Now().Add(-1 * time.Hour)
	flightEnd := time.Now().Add(48 * time.Hour)
	spon := h.AddPublisherLineItem(t, w.Publisher, "comp-f1-spon", harness.PubLineItemOpts{
		PriorityTier:  "sponsorship",
		Placements:    []harness.Placement{w.Placement},
		DeliveryStart: flightStart,
		DeliveryEnd:   flightEnd,
		CPM:           5.00,
		CreativeHTML:  `<div data-tier="sponsorship">f1-spon-marker</div>`,
	})
	h.RefreshAllCaches(t)

	resp := h.ServePubAd(t, w.Placement.ExternalID)
	if resp.Source != "direct" {
		t.Fatalf("source = %q; want direct (sponsorship should preempt the 50.00 programmatic bid)", resp.Source)
	}
	if resp.LineItemID != spon.ID {
		t.Errorf("LineItemID = %q; want sponsorship %q", resp.LineItemID, spon.ID)
	}
	if !strings.Contains(resp.HTML, "f1-spon-marker") {
		t.Errorf("HTML missing sponsorship marker; got %s", resp.HTML)
	}
}

// TestCompetitiveF4_OutOfFlightDirectSkippedProgrammaticWins — set up a
// sponsorship whose delivery_end is in the past; the publisher-adserver
// must skip it and fall through to the programmatic auction.
func TestCompetitiveF4_OutOfFlightDirectSkippedProgrammaticWins(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-f4")

	// Out-of-flight sponsorship (ended yesterday).
	flightEnd := time.Now().Add(-24 * time.Hour)
	flightStart := flightEnd.Add(-7 * 24 * time.Hour)
	_ = h.AddPublisherLineItem(t, w.Publisher, "comp-f4-spon-past", harness.PubLineItemOpts{
		PriorityTier:  "sponsorship",
		Placements:    []harness.Placement{w.Placement},
		DeliveryStart: flightStart,
		DeliveryEnd:   flightEnd,
		CPM:           25.00,
		CreativeHTML:  `<div data-tier="sponsorship">f4-past-marker</div>`,
	})
	h.RefreshAllCaches(t)

	// Programmatic should fill via the GBR/mobile campaign.
	resp := h.ServePubAdRaw(t, "placement_id="+w.Placement.ExternalID+"&geo=GBR&device=mobile")
	if resp.Source == "direct" {
		t.Fatalf("source = direct; want programmatic (sponsorship was out-of-flight)")
	}
	if resp.NoBid {
		t.Fatalf("expected programmatic fill; got nobid")
	}
	if strings.Contains(resp.HTML, "f4-past-marker") {
		t.Errorf("served the out-of-flight creative; html=%s", resp.HTML)
	}
}

// TestCompetitiveF5_DirectLineItemPlacementAllowlistRespected — create a
// sponsorship that's restricted to placement A. Request placement B (also
// under the same publisher) and verify the sponsorship is skipped, then
// programmatic fills. Proves the placement allowlist filter actually works.
func TestCompetitiveF5_DirectLineItemPlacementAllowlistRespected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-f5")

	// Second placement under the same publisher — the one we'll actually
	// request. The sponsorship below restricts to w.Placement only.
	other := h.AddPlacement(t, w.Publisher, "comp-f5-other", 300, 250, 1.00, []string{"IAB12"})

	flightStart := time.Now().Add(-1 * time.Hour)
	flightEnd := time.Now().Add(48 * time.Hour)
	_ = h.AddPublisherLineItem(t, w.Publisher, "comp-f5-spon-restricted", harness.PubLineItemOpts{
		PriorityTier:  "sponsorship",
		Placements:    []harness.Placement{w.Placement}, // restricted to the OTHER placement
		DeliveryStart: flightStart,
		DeliveryEnd:   flightEnd,
		CPM:           25.00,
		CreativeHTML:  `<div data-tier="sponsorship">f5-restricted-marker</div>`,
	})
	h.RefreshAllCaches(t)

	// Request the second placement — sponsorship doesn't apply, programmatic fills.
	resp := h.ServePubAdRaw(t, "placement_id="+other.ExternalID+"&geo=GBR&device=mobile")
	if resp.Source == "direct" {
		t.Fatalf("source = direct; sponsorship allowlist should have excluded placement %s",
			other.ExternalID)
	}
	if strings.Contains(resp.HTML, "f5-restricted-marker") {
		t.Errorf("served the placement-allowlisted creative on wrong placement; html=%s", resp.HTML)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PHASE 5 — deals competitive
// ─────────────────────────────────────────────────────────────────────────

// TestCompetitiveD2_PreferredDealRaisesFloorOpenBidStillWins — set up a
// preferred deal at 3.00 (raises the placement's effective floor) and run
// an auction where the test campaign bids 4.00. Open-market wins because
// 4.00 > 3.00 — preferred deals don't preempt, they only raise the floor.
// Asserts DealID is EMPTY (proves open-market path, not preferred path).
func TestCompetitiveD2_PreferredDealRaisesFloorOpenBidStillWins(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-d2")
	// No WithDeterministicCompetitors: BuildBasicWorld wipes comp1/comp2
	// campaigns; only the BasicWorld internal-DSP campaign bids. Noise
	// has no surface.

	// Preferred deal at 3.00. Allowlisted = our test advertiser.
	_ = h.CreateDeal(t, w.Publisher, "comp-d2-deal", "preferred", 3.00,
		[]string{w.AdvAcc.ID}, []string{w.Placement.ID})

	// Bid 4.00 — above the 3.00 preferred floor.
	h.SetCampaignBaseBid(t, w.Campaign, 4.00)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-d2-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected open-market win at 4.00 above preferred floor 3.00")
	}
	if win.Price < 3.0 {
		t.Errorf("clearing price %v < preferred floor 3.0", win.Price)
	}
}

// TestCompetitiveD3_PMPAllowlistedAtDealPriceVsNonListedOpen — PMP deal
// with our test advertiser allowlisted at deal price 2.00. The test
// campaign bids 2.50 (above deal price). Asserts DealID is set on the
// winning bid (proves PMP path ran). Comment above mentioned "competitor
// DSPs are also bidding" — that's aspirational: BuildBasicWorld wipes
// comp1/comp2 campaigns, so in this test only internal-DSP's BasicWorld
// campaign bids and the PMP-vs-open competition collapses to a single
// bidder. Still proves the PMP code path runs.
func TestCompetitiveD3_PMPAllowlistedAtDealPriceVsNonListedOpen(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "comp-d3")
	// No WithDeterministicCompetitors: comp1/comp2 have no campaigns
	// post-BuildBasicWorld; noise has no surface.

	_ = h.CreateDeal(t, w.Publisher, "comp-d3-deal", "pmp", 2.00,
		[]string{w.AdvAcc.ID}, []string{w.Placement.ID})

	h.SetCampaignBaseBid(t, w.Campaign, 2.50)
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "comp-d3-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected a winning bid")
	}
	if win.DealID == "" {
		t.Errorf("winner DealID empty — PMP path didn't fire (winner=%+v)", win)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PHASE 6 — Prebid competitive
// ─────────────────────────────────────────────────────────────────────────

// TestCompetitiveG1_PrebidMultiImpRequestPerImpAuction — a single Prebid
// request can carry multiple impressions (e.g., 3 ad slots on one page
// loaded together). Each imp should get its own auction. We send 2 imps,
// one against our matchable placement, one against a non-existent
// placement. Expect: 1 SeatBid for the valid imp, no bid for the other.
//
// Current behaviour + why this is deferred (after reading both hot paths):
// BOTH the exchange auctionHandler (cmd/exchange/main.go ~500-778) AND the DSP
// bid handler (cmd/dsp/main.go ~896-1141) hardcode bidReq.Imp[0] end-to-end —
// floor, format, placement, deal eval, the single-winner auction, the response
// BidObj.ImpID, win/loss, and the AuctionWin/Complete events. `auction.Bid`
// carries no ImpID, so bids can't even be grouped per-imp without a core-type
// change.
//
// The real blocker isn't the two handlers — it's the trace_id/billing model.
// AuctionWinEvent is "the single source of truth for cost", keyed on trace_id,
// and the tracker/billing dedup on trace_id. If one request wins N imps (N ads
// rendered), each win needs its OWN trace_id threaded through render → track →
// bill, or the two impressions collapse to one cost. So correct per-imp support
// is a PIPELINE-WIDE change (exchange, DSP, SSP, ad server, tracker, billing +
// the trace-id-per-imp model) that must preserve the one-AuctionWinEvent-per-
// cost invariant — a dedicated project, not a tail-end gap fill. Rushing it
// risks the core money invariant, so it stays deferred.
func TestCompetitiveG1_PrebidMultiImpRequestPerImpAuction(t *testing.T) {
	t.Skip("deferred: correct Prebid multi-imp needs a per-imp trace_id threaded through the whole render→track→bill pipeline to preserve the one-AuctionWinEvent-per-cost invariant — see comment")
}

// TestCompetitiveG2_PrebidInboundVsInternalDSPsHighestWins — inbound
// Prebid request matching our seeded inventory; the exchange's normal
// DSP fan-out fires alongside. With deterministic competitors, comp1's
// MegaStore (4.55 effective) should outbid internal's Acme Shoes (2.50).
// Confirms Prebid inbound + internal fan-out compose correctly.
func TestCompetitiveG2_PrebidInboundVsInternalDSPsHighestWins(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.SeedStandard(t)
	h.WithDeterministicCompetitors(t)
	h.RefreshAllCaches(t)

	req := openrtb.BidRequest{
		ID: "comp-g2-trace",
		Imp: []openrtb.Imp{{
			ID:       "imp-1",
			TagID:    placementUUIDFromExternal("pl-news-mpu"),
			BidFloor: 0.50,
			Banner:   &openrtb.Banner{W: 300, H: 250},
		}},
		Site: &openrtb.Site{
			Domain:    "daily-news.com",
			Publisher: &openrtb.Publisher{ID: publisherUUIDFromExternal("pub-daily-news")},
		},
		Device: &openrtb.Device{
			Geo:        &openrtb.Geo{Country: "GBR"},
			DeviceType: 1,
		},
		TMax: 500,
	}
	resp, status := h.PostPrebidAuction(t, req)
	if status != 200 {
		t.Fatalf("prebid status = %d; want 200", status)
	}
	if resp.NoBid {
		t.Fatal("expected a winning bid (3 deterministic DSPs, comp1 MegaStore should win)")
	}
	if len(resp.SeatBid) == 0 || len(resp.SeatBid[0].Bid) == 0 {
		t.Fatalf("missing bid in response: %+v", resp)
	}
	price := resp.SeatBid[0].Bid[0].Price
	if price < 4.0 {
		t.Errorf("clearing price %v < 4.0 — competitor1 didn't win, internal (~2.50) did?", price)
	}
}

// placementUUIDFromExternal / publisherUUIDFromExternal are helpers that
// re-derive UUIDs from external keys at test time, matching idgen.Derive.
// Kept local to this file because the only callers are the Prebid tests
// that need to construct OpenRTB requests with raw UUIDs (the auction
// matcher needs UUIDs, not external keys).
func placementUUIDFromExternal(key string) string { return deriveID("placement", key) }
func publisherUUIDFromExternal(key string) string { return deriveID("publisher", key) }
