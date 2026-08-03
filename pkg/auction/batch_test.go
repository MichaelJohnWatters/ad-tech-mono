package auction_test

import (
	"context"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// N surfaces in one scene → top N bids from DISTINCT advertisers each take a
// surface, ranked by price, positions 1..N.
func TestBatch_FillsSceneSurfaces(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "a", AdvertiserID: "adv-a", Price: 2.00},
		{DSPID: "d2", CampaignID: "b", AdvertiserID: "adv-b", Price: 5.00},
		{DSPID: "d3", CampaignID: "c", AdvertiserID: "adv-c", Price: 3.00},
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel: "ingame", Format: "intrinsic", SlotCount: 3,
	})
	if err != nil {
		t.Fatalf("in-game auction errored: %v", err)
	}
	if res.StrategyType != "batch" {
		t.Errorf("strategy = %q, want batch", res.StrategyType)
	}
	if !res.IsMultiWinner {
		t.Error("in-game result should be flagged multi-winner")
	}
	if len(res.Winners) != 3 {
		t.Fatalf("want 3 surfaces filled, got %d", len(res.Winners))
	}
	// Price-ranked b(5) > c(3) > a(2), positions 1..3, each first-price.
	want := []struct {
		cid   string
		pos   int
		price float64
	}{{"b", 1, 5.00}, {"c", 2, 3.00}, {"a", 3, 2.00}}
	for i, w := range want {
		got := res.Winners[i]
		if got.Bid.CampaignID != w.cid || got.Position != w.pos || got.ClearingPrice != w.price {
			t.Errorf("surface %d = %s/pos%d/$%.2f, want %s/pos%d/$%.2f",
				i, got.Bid.CampaignID, got.Position, got.ClearingPrice, w.cid, w.pos, w.price)
		}
	}
}

// Competitive separation: one advertiser per scene. An advertiser's second
// (even higher) product cannot take a second surface.
func TestBatch_OneAdvertiserPerScene(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "cola-a", AdvertiserID: "cola", Price: 9.00}, // Cola's top product
		{DSPID: "d1", CampaignID: "cola-b", AdvertiserID: "cola", Price: 8.00}, // Cola's 2nd — must be blocked
		{DSPID: "d2", CampaignID: "pepsi", AdvertiserID: "pepsi", Price: 4.00},
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel: "ingame", Format: "intrinsic", SlotCount: 3,
	})
	if err != nil {
		t.Fatalf("in-game auction errored: %v", err)
	}
	// Cola takes one surface (its $9), Pepsi the other; Cola's $8 is blocked, so
	// only 2 surfaces fill and one is left for a house ad.
	if len(res.Winners) != 2 {
		t.Fatalf("want 2 winners (one per advertiser), got %d: %+v", len(res.Winners), res.Winners)
	}
	advs := map[string]int{}
	for _, w := range res.Winners {
		advs[w.Bid.AdvertiserID]++
	}
	if advs["cola"] != 1 || advs["pepsi"] != 1 {
		t.Errorf("each advertiser should hold exactly one surface, got %v", advs)
	}
	if res.ShortFill != 1 {
		t.Errorf("ShortFill = %d, want 1 (3 surfaces, 2 non-competing advertisers)", res.ShortFill)
	}
	// The blocked second Cola product is recorded as a competitive-separation loss.
	var blocked bool
	for _, l := range res.LossBids {
		if l.Bid.CampaignID == "cola-b" && l.Reason == auction.LossBlocked {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("cola-b should be a LossBlocked (competitive separation), losses=%+v", res.LossBids)
	}
}

// Competitive separation also dedupes by category (competing brands in one shot).
func TestBatch_OneCategoryPerScene(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "energy-1", AdvertiserID: "adv-a", Category: "IAB8", Price: 6.00},
		{DSPID: "d2", CampaignID: "energy-2", AdvertiserID: "adv-b", Category: "IAB8", Price: 5.00}, // same category → blocked
		{DSPID: "d3", CampaignID: "car", AdvertiserID: "adv-c", Category: "IAB2", Price: 4.00},
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel: "ingame", Format: "intrinsic", SlotCount: 3,
	})
	if err != nil {
		t.Fatalf("in-game auction errored: %v", err)
	}
	if len(res.Winners) != 2 {
		t.Fatalf("want 2 winners (one per category), got %d", len(res.Winners))
	}
	if res.Winners[0].Bid.CampaignID != "energy-1" || res.Winners[1].Bid.CampaignID != "car" {
		t.Errorf("want energy-1 + car (distinct categories), got %s + %s",
			res.Winners[0].Bid.CampaignID, res.Winners[1].Bid.CampaignID)
	}
}
