//go:build e2e

// Functionality e2e suite for the ad-tech-mono platform.
//
// Assumes `tilt up` is running. Each TestEndToEnd run resets state and walks
// the full lifecycle as compounding subtests — if step N fails, steps N+1..
// are correctly reported as cascade failures (they depend on N).
//
// Run: `make test-e2e`
// Run a single step: `go test -tags=e2e -run TestEndToEnd/07_first_auction ./tests/e2e/...`
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestEndToEnd(t *testing.T) {
	// Wait for every service then start from a clean DB / Redis.
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	// Shared state accumulated across subtests. Pointer-by-value means a
	// failed subtest still leaves valid handles for downstream steps to
	// either reference or skip via t.Skip / cascade.
	var (
		admin          harness.Account
		pubAccount     harness.Account
		advAccount     harness.Account
		publisher      harness.Publisher
		placement      harness.Placement
		insertionOrder harness.InsertionOrder
		campaign       harness.Campaign
		auctionRes     harness.AuctionResult
		winner         harness.BidResponseWinner
	)

	t.Run("01_admin_signup", func(t *testing.T) {
		admin = h.CreateAdmin(t, "e2e-admin")
		if !h.AccountExists(t, admin.ID) {
			t.Fatal("admin row not present after signup")
		}
	})

	t.Run("02_publisher_account", func(t *testing.T) {
		pubAccount = h.CreatePublisher(t, "e2e-pub-news")
	})

	t.Run("03_publisher_inventory", func(t *testing.T) {
		publisher = h.AddPublisher(t, pubAccount, "e2e-pub-news", "e2e-news.test")
		placement = h.AddPlacement(t, publisher, "e2e-pl-mpu",
			300, 250, 1.00, []string{"IAB12"})
	})

	t.Run("04_advertiser_account", func(t *testing.T) {
		// "adv-acme" matches the internal DSP profile's allowlist
		// (profiles/dsps/internal.yaml) so the campaign lands in the DSP
		// warm cache. See harness.BuildBasicWorld for the same trick.
		advAccount = h.CreateAdvertiser(t, "adv-acme")
		// Fund the account: the DSP balance gate fails closed for accounts
		// with no advertiser_balances row (prepay posture), so an unfunded
		// advertiser can't win the auction in step 07.
		h.GrantBalance(t, advAccount.ID, 100_000, "e2e-narrative-grant")
	})

	t.Run("05_campaign_setup", func(t *testing.T) {
		insertionOrder = h.CreateInsertionOrder(t, advAccount, "e2e-io-acme-q1", 5000)
		campaign = h.CreateCampaign(t, advAccount, insertionOrder,
			"e2e-li-acme-uk-mobile",
			3.50, // base bid
			500,  // daily budget
			"e2e-cr-acme-mpu",
			"acme.test",
			harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}},
		)
	})

	t.Run("06_caches_populated", func(t *testing.T) {
		// Force every warm cache to reload synchronously from Postgres.
		// Without this, step 07 would race the 30s poll interval — tests
		// would either flake or have to WaitFor up to 35s.
		h.RefreshAllCaches(t)

		// Now assertions can be immediate. DSP allowlist filtering means
		// the new advertiser's campaign should appear in whichever DSP pod
		// has no managed-advertiser restriction (internal at 8082 by default).
		if ids := h.DSPCampaignIDs(t, h.URLs.DSP); !contains(ids, campaign.ID) {
			t.Errorf("DSP campaigns do not contain %q after refresh; got %v", campaign.ID, ids)
		}
		if ids := h.SSPPlacementIDs(t); !contains(ids, placement.ID) {
			t.Errorf("SSP placements do not contain %q after refresh; got %v", placement.ID, ids)
		}
	})

	t.Run("07_first_auction", func(t *testing.T) {
		auctionRes = h.RunAuction(t, placement.ExternalID, "GBR", "mobile", "user-001")
		winner = h.ExtractWinner(t, auctionRes)
		if winner.NoBid {
			t.Fatalf("expected a winning bid, got no_bid. response: %s", string(auctionRes.BidResponse))
		}
		if winner.Seat != advAccount.ID {
			t.Errorf("winning seat = %q, want advertiser %q", winner.Seat, advAccount.ID)
		}
		if winner.CampaignID != campaign.ID {
			t.Errorf("winning campaign = %q, want %q", winner.CampaignID, campaign.ID)
		}
		if winner.Price < placement.FloorPrice {
			t.Errorf("clearing price %.4f below floor %.4f", winner.Price, placement.FloorPrice)
		}
	})

	t.Run("08_tracker_and_billing", func(t *testing.T) {
		// In-memory ledger accumulates across the suite (Reset truncates
		// Postgres + flushes Redis but not the in-process ledger), so we
		// snapshot before and assert on the delta — robust to whatever
		// other tests ran first.
		spendBefore := totalSpend(t, h)

		h.FireImpression(t,
			auctionRes.TraceID,
			campaign.ID, campaign.CreativeID,
			placement.ID, publisher.ID, advAccount.ID,
			"USD", winner.Price,
		)

		// Billing accrual flows: tracker → NATS impression event → reporting
		// (which hosts the billing engine) → billing.Summary surfaces spend.
		// Wait up to 5s for the async chain to deliver an impression-priced
		// delta in the summary.
		harness.WaitFor(t, 5*time.Second, "billing summary to reflect impression", func() bool {
			return totalSpend(t, h)-spendBefore > 0
		})
	})

	// ------------------------------------------------------------
	// Steps 09-14: deal preempt, pause + cache invalidate, freq cap.
	// Each step still depends on the state built up by 01-08.
	// ------------------------------------------------------------

	const pgDealPrice = 6.50
	var pgDealID string

	t.Run("09_create_pg_deal", func(t *testing.T) {
		// PG deal: this publisher × our advertiser × this placement, price
		// 6.50 (above the DSP's 3.50 base bid, so the deal floor visibly
		// dominates the clearing price in step 10).
		pgDealID = h.CreateDeal(t, publisher, "e2e-pg-acme",
			"pg", pgDealPrice,
			[]string{advAccount.ID},
			[]string{placement.ID},
		)
		h.RefreshAllCaches(t)

		if ids := h.ExchangeDealIDs(t); !contains(ids, pgDealID) {
			t.Errorf("exchange deal cache missing PG deal %q; got %v", pgDealID, ids)
		}
	})

	t.Run("10_pg_deal_preempts_auction", func(t *testing.T) {
		res := h.RunAuction(t, placement.ExternalID, "GBR", "mobile", "user-002")
		w := h.ExtractWinner(t, res)
		if w.NoBid {
			t.Fatal("expected winning bid after PG match; got no_bid")
		}
		if w.DealID != pgDealID {
			t.Errorf("winner DealID = %q, want %q (PG deal should be on the winning bid)", w.DealID, pgDealID)
		}
		// PG sets EffectiveFloor = max(placement_floor, deal.price) which is
		// what becomes the clearing price on preempt. Floor here is 1.00,
		// deal price is 6.50, so clearing should be 6.50.
		if w.Price != pgDealPrice {
			t.Errorf("clearing price = %.4f, want %.4f (PG deal price)", w.Price, pgDealPrice)
		}
	})

	t.Run("11_pause_campaign_via_db", func(t *testing.T) {
		h.PauseCampaign(t, campaign)
		h.RefreshAllCaches(t)
	})

	t.Run("12_paused_campaign_no_bids", func(t *testing.T) {
		// CampaignLoader pulls both live + paused (so admin tooling can
		// see paused), but the DSP bid handler skips Status != "live". With
		// only our paused campaign in scope (DSP allowlist on the internal
		// pod), there are no live campaigns → no_bid.
		res := h.RunAuction(t, placement.ExternalID, "GBR", "mobile", "user-003")
		w := h.ExtractWinner(t, res)
		if !w.NoBid {
			t.Errorf("expected no_bid for paused campaign, got winner DSP %q at %.4f", w.Seat, w.Price)
		}

		// Restore live for subsequent steps so the freq cap test has a real
		// campaign to bid on. ResumeCampaign + RefreshAllCaches mirrors what
		// a "resume campaign" admin action would do.
		h.ResumeCampaign(t, campaign)
		h.RefreshAllCaches(t)
	})

	t.Run("13_nats_invalidate_propagates", func(t *testing.T) {
		// Production code path: writes through Gateway publish on the
		// invalidate subject and warm caches reload without a refresh call.
		// Mutate the DB with a distinctive budget value, publish the
		// invalidate, then assert the new value (not just presence) lands
		// in the DSP cache — presence alone would pass even with NATS down,
		// since the campaign was cached in step 06.
		const newBudget = 9999.0
		h.SetCampaignDailyBudget(t, campaign, newBudget)
		h.PublishInvalidate(t, events.SubjectCacheInvalidateCampaigns)

		// 10s window: NATS deliver + warm cache reload + Postgres roundtrip
		// is usually <100ms in isolation, but under full-suite load (many
		// JetStream consumers + parallel cache refreshes from prior tests)
		// can stretch closer to a few seconds. 3s was tight enough to flake
		// when this test runs late in the suite.
		harness.WaitFor(t, 10*time.Second, "DSP cache to reflect new budget after NATS invalidate", func() bool {
			b, ok := h.DSPCampaignBudget(t, h.URLs.DSP, campaign.ID)
			return ok && b == newBudget
		})
	})

	t.Run("14_freq_cap_blocks_after_limit", func(t *testing.T) {
		// Default freq cap is 5 per (user_id, campaign_id) per 24h window
		// (adserver.freq_cap_per_user_per_campaign). The 6th call must 429.
		// Use a unique user_id to avoid colliding with earlier auctions.
		req := models.ServeRequest{
			TraceID:       "e2e-freqcap-trace",
			CampaignID:    campaign.ID,
			CreativeID:    campaign.CreativeID,
			PlacementID:   placement.ID,
			PublisherID:   publisher.ID,
			AdvertiserID:  advAccount.ID,
			ClearingPrice: 1.00,
			Currency:      "USD",
			SiteDomain:    publisher.Domain,
			Width:         300,
			Height:        250,
			UserID:        "e2e-user-freqcap",
		}
		for i := 1; i <= 5; i++ {
			if status := h.ServeAd(t, req); status != 200 {
				t.Fatalf("serve call %d status = %d, want 200 (under cap)", i, status)
			}
		}
		if status := h.ServeAd(t, req); status != 429 {
			t.Errorf("6th serve call status = %d, want 429 (freq cap exceeded)", status)
		}
	})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
