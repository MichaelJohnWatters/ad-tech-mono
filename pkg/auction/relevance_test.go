package auction_test

import (
	"context"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// The defining retail-media property: relevance × bid, so a relevant product
// bidding LESS outranks an irrelevant product bidding MORE.
func TestRelevanceWeighted_RelevanceBeatsHigherBid(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "shoes", Price: 2.00, Category: "IAB18-5"},   // relevant, cheap → 1.0×2 = 2.0
		{DSPID: "d2", CampaignID: "loans", Price: 5.00, Category: "IAB13"},     // irrelevant, pricey → 0.1×5 = 0.5
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel:          "retail",
		SlotCount:        1,
		RetailCategories: []string{"IAB18-5"}, // shopper is browsing shoes
	})
	if err != nil {
		t.Fatalf("retail auction errored: %v", err)
	}
	if res.StrategyType != "relevance_weighted" {
		t.Errorf("strategy = %q, want relevance_weighted", res.StrategyType)
	}
	if !res.IsMultiWinner {
		t.Error("retail result should be flagged multi-winner")
	}
	if len(res.Winners) != 1 || res.Winners[0].Bid.CampaignID != "shoes" {
		t.Fatalf("want the relevant cheaper product 'shoes' to win, got %+v", res.Winners)
	}
	// GSP: shoes pays only the minimum to hold rank 1 = loans' score (0.1×5=0.5)
	// divided by shoes' relevance (1.0) = 0.50 — NOT its own 2.00 bid.
	if res.Winners[0].ClearingPrice != 0.50 {
		t.Errorf("GSP clearing = %v, want 0.50 (next score / own relevance)", res.Winners[0].ClearingPrice)
	}
	if res.Winners[0].Position != 1 {
		t.Errorf("winner position = %d, want 1", res.Winners[0].Position)
	}
}

// GSP ladder across multiple slots: each slot pays the next-ranked score / its
// own relevance; the last filled slot clears at the floor.
func TestRelevanceWeighted_GSPLadder(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	// All in-category (relevance 1.0), so score == bid: a=5, b=3, c=2.
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "a", Price: 5.00, Category: "IAB18"},
		{DSPID: "d2", CampaignID: "b", Price: 3.00, Category: "IAB18"},
		{DSPID: "d3", CampaignID: "c", Price: 2.00, Category: "IAB18"},
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel: "retail", SlotCount: 2, RetailCategories: []string{"IAB18"}, FloorPrice: 0.50,
	})
	if err != nil {
		t.Fatalf("retail auction errored: %v", err)
	}
	// a (pos1) pays b's score 3.00; b (pos2) pays c's score 2.00.
	if res.Winners[0].ClearingPrice != 3.00 {
		t.Errorf("pos1 GSP = %v, want 3.00 (next score)", res.Winners[0].ClearingPrice)
	}
	if res.Winners[1].ClearingPrice != 2.00 {
		t.Errorf("pos2 GSP = %v, want 2.00 (next score)", res.Winners[1].ClearingPrice)
	}
}

// MinRelevance floor: a product below the floor doesn't show, even bidding high.
func TestRelevanceWeighted_MinRelevanceFloor(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "shoes", Price: 2.00, Category: "IAB18-5"}, // rel 1.0
		{DSPID: "d2", CampaignID: "loans", Price: 50.00, Category: "IAB13"},  // rel 0.1 — below floor
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel: "retail", SlotCount: 2, RetailCategories: []string{"IAB18-5"}, MinRelevance: 0.5,
	})
	if err != nil {
		t.Fatalf("retail auction errored: %v", err)
	}
	if len(res.Winners) != 1 || res.Winners[0].Bid.CampaignID != "shoes" {
		t.Fatalf("only 'shoes' clears the relevance floor, got %+v", res.Winners)
	}
	var blocked bool
	for _, l := range res.LossBids {
		if l.Bid.CampaignID == "loans" && l.Reason == auction.LossBlocked {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("'loans' should be blocked below MinRelevance, losses=%+v", res.LossBids)
	}
}

// N sponsored slots → top-N ranked winners with positions 1..N; the rest lose;
// ShortFill counts unfilled slots when demand < slots.
func TestRelevanceWeighted_MultiSlotPositionsAndShortFill(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	// All same category as the request → relevance neutral, so pure price order.
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "a", Price: 1.00, Category: "IAB18"},
		{DSPID: "d2", CampaignID: "b", Price: 3.00, Category: "IAB18"},
		{DSPID: "d3", CampaignID: "c", Price: 2.00, Category: "IAB18"},
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel:          "retail",
		SlotCount:        4, // more slots than eligible products
		RetailCategories: []string{"IAB18"},
	})
	if err != nil {
		t.Fatalf("retail auction errored: %v", err)
	}
	if len(res.Winners) != 3 {
		t.Fatalf("want 3 winners (3 products fill 3 of 4 slots), got %d", len(res.Winners))
	}
	// Ranked b(3.00) > c(2.00) > a(1.00), positions 1..3.
	wantOrder := []struct {
		cid string
		pos int
	}{{"b", 1}, {"c", 2}, {"a", 3}}
	for i, w := range wantOrder {
		if res.Winners[i].Bid.CampaignID != w.cid || res.Winners[i].Position != w.pos {
			t.Errorf("slot %d = %s/pos%d, want %s/pos%d", i, res.Winners[i].Bid.CampaignID, res.Winners[i].Position, w.cid, w.pos)
		}
	}
	if res.ShortFill != 1 {
		t.Errorf("ShortFill = %d, want 1 (4 slots, 3 products)", res.ShortFill)
	}
}

// Beyond SlotCount, extra eligible bids are recorded as losses (outranked).
func TestRelevanceWeighted_ExtraBidsLose(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "a", Price: 5.00},
		{DSPID: "d2", CampaignID: "b", Price: 4.00},
		{DSPID: "d3", CampaignID: "c", Price: 3.00},
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel:   "retail",
		SlotCount: 2, // no RetailCategories → pure price ranking
	})
	if err != nil {
		t.Fatalf("retail auction errored: %v", err)
	}
	if len(res.Winners) != 2 || res.Winners[0].Bid.CampaignID != "a" || res.Winners[1].Bid.CampaignID != "b" {
		t.Fatalf("want winners a,b, got %+v", res.Winners)
	}
	if res.ShortFill != 0 {
		t.Errorf("ShortFill = %d, want 0 (2 slots, 3 products)", res.ShortFill)
	}
	// The one product that didn't get a slot is a loss (outranked), plus any
	// filter losses — assert the outranked one is present.
	var outranked bool
	for _, l := range res.LossBids {
		if l.Bid.CampaignID == "c" && l.Reason == auction.LossOutbid {
			outranked = true
		}
	}
	if !outranked {
		t.Errorf("product 'c' should be recorded as an outranked loss, losses=%+v", res.LossBids)
	}
}

// An explicit Bid.Relevance (e.g. an ML score) overrides category derivation.
func TestRelevanceWeighted_ExplicitRelevanceOverride(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{
		{DSPID: "d1", CampaignID: "hi-rel", Price: 1.00, Relevance: 0.9}, // 0.9×1 = 0.9
		{DSPID: "d2", CampaignID: "lo-rel", Price: 2.00, Relevance: 0.2}, // 0.2×2 = 0.4
	}
	res, err := engine.RunAuction(context.Background(), bids, auction.AuctionRequest{
		Channel:   "retail",
		SlotCount: 1,
		// RetailCategories intentionally empty — explicit Relevance must still apply.
	})
	if err != nil {
		t.Fatalf("retail auction errored: %v", err)
	}
	if res.Winners[0].Bid.CampaignID != "hi-rel" {
		t.Fatalf("explicit relevance should win for hi-rel, got %+v", res.Winners)
	}
}
