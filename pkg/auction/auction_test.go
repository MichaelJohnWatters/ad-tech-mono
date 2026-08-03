package auction_test

import (
	"context"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

func TestSingleWinner_FirstPrice(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})

	bids := []auction.Bid{
		{DSPID: "dsp_1", CampaignID: "camp_1", Price: 2.50, AdvertiserID: "adv_1"},
		{DSPID: "dsp_2", CampaignID: "camp_2", Price: 3.00, AdvertiserID: "adv_2"},
		{DSPID: "dsp_3", CampaignID: "camp_3", Price: 1.80, AdvertiserID: "adv_3"},
	}

	request := auction.AuctionRequest{
		Channel:    "display",
		PriceMode:  "first_price",
		FloorPrice: 1.00,
		TraceID:    "trace-1",
	}

	result, err := engine.RunAuction(context.Background(), bids, request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Winners) != 1 {
		t.Fatalf("expected 1 winner, got %d", len(result.Winners))
	}

	winner := result.Winners[0]
	if winner.Bid.DSPID != "dsp_2" {
		t.Errorf("winner DSPID = %s, want dsp_2 (highest bid)", winner.Bid.DSPID)
	}
	if winner.ClearingPrice != 3.00 {
		t.Errorf("clearing price = %f, want 3.00 (first-price)", winner.ClearingPrice)
	}
	if result.StrategyType != "single_winner" {
		t.Errorf("strategy type = %s, want single_winner", result.StrategyType)
	}
}

func TestSingleWinner_SecondPrice(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})

	bids := []auction.Bid{
		{DSPID: "dsp_1", CampaignID: "camp_1", Price: 5.00, AdvertiserID: "adv_1"},
		{DSPID: "dsp_2", CampaignID: "camp_2", Price: 3.00, AdvertiserID: "adv_2"},
		{DSPID: "dsp_3", CampaignID: "camp_3", Price: 2.00, AdvertiserID: "adv_3"},
	}

	request := auction.AuctionRequest{
		Channel:    "display",
		PriceMode:  "second_price",
		FloorPrice: 1.00,
		TraceID:    "trace-2",
	}

	result, err := engine.RunAuction(context.Background(), bids, request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	winner := result.Winners[0]
	if winner.Bid.DSPID != "dsp_1" {
		t.Errorf("winner = %s, want dsp_1", winner.Bid.DSPID)
	}
	// Second price = second highest bid + $0.01
	if winner.ClearingPrice != 3.01 {
		t.Errorf("clearing price = %f, want 3.01 (second-price)", winner.ClearingPrice)
	}
}

func TestSingleWinner_SecondPrice_OneBid(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})

	bids := []auction.Bid{
		{DSPID: "dsp_1", CampaignID: "camp_1", Price: 5.00, AdvertiserID: "adv_1"},
	}

	request := auction.AuctionRequest{
		Channel:    "display",
		PriceMode:  "second_price",
		FloorPrice: 2.00,
		TraceID:    "trace-3",
	}

	result, err := engine.RunAuction(context.Background(), bids, request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With one bid, second-price falls back to floor price
	if result.Winners[0].ClearingPrice != 2.00 {
		t.Errorf("clearing price = %f, want 2.00 (floor, only one bid)", result.Winners[0].ClearingPrice)
	}
}

func TestAuction_FloorPriceFilter(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})

	bids := []auction.Bid{
		{DSPID: "dsp_1", CampaignID: "camp_1", Price: 0.50, AdvertiserID: "adv_1"},
		{DSPID: "dsp_2", CampaignID: "camp_2", Price: 0.30, AdvertiserID: "adv_2"},
	}

	request := auction.AuctionRequest{
		Channel:    "display",
		PriceMode:  "first_price",
		FloorPrice: 1.00,
		TraceID:    "trace-4",
	}

	_, err := engine.RunAuction(context.Background(), bids, request)
	if err != auction.ErrNoBids {
		t.Errorf("expected ErrNoBids, got %v", err)
	}
}

func TestAuction_NoBids(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})

	request := auction.AuctionRequest{
		Channel:    "display",
		PriceMode:  "first_price",
		FloorPrice: 1.00,
	}

	_, err := engine.RunAuction(context.Background(), nil, request)
	if err != auction.ErrNoBids {
		t.Errorf("expected ErrNoBids, got %v", err)
	}
}

func TestAuction_LossBids(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})

	bids := []auction.Bid{
		{DSPID: "dsp_1", CampaignID: "camp_1", Price: 3.00, AdvertiserID: "adv_1"},
		{DSPID: "dsp_2", CampaignID: "camp_2", Price: 2.00, AdvertiserID: "adv_2"},
		{DSPID: "dsp_3", CampaignID: "camp_3", Price: 0.50, AdvertiserID: "adv_3"}, // below floor
	}

	request := auction.AuctionRequest{
		Channel:    "display",
		PriceMode:  "first_price",
		FloorPrice: 1.00,
		TraceID:    "trace-5",
	}

	result, err := engine.RunAuction(context.Background(), bids, request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// dsp_2 lost (outbid), dsp_3 lost (below floor)
	if len(result.LossBids) != 2 {
		t.Fatalf("expected 2 loss bids, got %d", len(result.LossBids))
	}

	// Check loss reasons
	hasOutbid := false
	hasBelowFloor := false
	for _, loss := range result.LossBids {
		if loss.Reason == auction.LossOutbid {
			hasOutbid = true
		}
		if loss.Reason == auction.LossBelowFloor {
			hasBelowFloor = true
		}
	}
	if !hasOutbid {
		t.Error("expected a LossOutbid loss bid")
	}
	if !hasBelowFloor {
		t.Error("expected a LossBelowFloor loss bid")
	}
}

func TestAuction_SelectStrategy(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})

	tests := []struct {
		channel  string
		format   string
		expected string
	}{
		{"display", "banner", "single_winner"},
		{"native", "native", "single_winner"},
		{"video", "single", "single_winner"},
		{"video", "pod", "pod"},
		{"audio", "single", "single_winner"},
		{"dooh", "", "timeslot"},
		{"retail", "", "relevance_weighted"},
		{"ingame", "rewarded", "single_winner"},
		{"ingame", "intrinsic", "batch"},
	}

	for _, tt := range tests {
		request := auction.AuctionRequest{Channel: tt.channel, Format: tt.format}
		strategy, err := engine.SelectStrategy(request)
		if err != nil {
			t.Errorf("SelectStrategy(%s/%s) error: %v", tt.channel, tt.format, err)
			continue
		}
		if strategy.Type() != tt.expected {
			t.Errorf("SelectStrategy(%s/%s) = %s, want %s", tt.channel, tt.format, strategy.Type(), tt.expected)
		}
	}
}

// Every channel strategy — single_winner, pod, timeslot, relevance_weighted,
// batch — is now implemented and covered by its own test. No strategy returns
// ErrNotImplemented, so the former TestStubbedStrategies_ReturnNotImplemented is
// gone. This assertion pins that: nothing routed by SelectStrategy stays stubbed.
func TestNoStubbedStrategiesRemain(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{{DSPID: "dsp_1", CampaignID: "c1", AdvertiserID: "adv", Price: 5.00}}
	channels := []struct{ channel, format string }{
		{"display", "banner"}, {"video", "pod"}, {"dooh", ""},
		{"retail", ""}, {"ingame", "intrinsic"},
	}
	for _, c := range channels {
		_, err := engine.RunAuction(context.Background(), bids,
			auction.AuctionRequest{Channel: c.channel, Format: c.format, SlotCount: 1, FloorPrice: 0})
		if err == auction.ErrNotImplemented {
			t.Errorf("channel %s/%s still returns ErrNotImplemented", c.channel, c.format)
		}
	}
}

// TestTimeSlotDOOHSingleWinner: a DOOH screen request fills its slot with the top
// bid (single winner per play; the audience multiplier applies downstream at
// proof-of-play, not in the auction).
func TestTimeSlotDOOHSingleWinner(t *testing.T) {
	engine := auction.NewEngine(clock.Real{})
	bids := []auction.Bid{
		{DSPID: "dsp_1", CampaignID: "c1", Price: 4.00},
		{DSPID: "dsp_2", CampaignID: "c2", Price: 6.50},
		{DSPID: "dsp_3", CampaignID: "c3", Price: 5.00},
	}
	res, err := engine.RunAuction(context.Background(), bids,
		auction.AuctionRequest{Channel: "dooh", FloorPrice: 1.0, PriceMode: "first_price"})
	if err != nil {
		t.Fatalf("DOOH auction errored: %v", err)
	}
	if res.StrategyType != "timeslot" {
		t.Errorf("strategy = %q, want timeslot", res.StrategyType)
	}
	if len(res.Winners) != 1 || res.Winners[0].Bid.CampaignID != "c2" {
		t.Fatalf("want single winner c2 (highest), got %+v", res.Winners)
	}
	if res.Winners[0].ClearingPrice != 6.50 {
		t.Errorf("first-price clearing = %v, want 6.50", res.Winners[0].ClearingPrice)
	}
}

func TestSeparationContext(t *testing.T) {
	sc := auction.NewSeparationContext()

	// Record Coca-Cola winning slot 1
	sc.RecordWinner("coca_cola", "beverages")

	// Pepsi should be blocked (same category)
	if !sc.IsBlocked("pepsi", "beverages") {
		t.Error("Pepsi should be blocked (same category as Coca-Cola)")
	}

	// Coca-Cola should be blocked (self-separation)
	if !sc.IsBlocked("coca_cola", "beverages") {
		t.Error("Coca-Cola should be blocked (already on page)")
	}

	// Nike should NOT be blocked (different category)
	if sc.IsBlocked("nike", "sportswear") {
		t.Error("Nike should NOT be blocked (different category)")
	}
}

func TestFilterBidsWithSeparation(t *testing.T) {
	sc := auction.NewSeparationContext()
	sc.RecordWinner("coca_cola", "beverages")

	bids := []auction.Bid{
		{DSPID: "dsp_1", AdvertiserID: "pepsi", Category: "beverages", Price: 5.00},
		{DSPID: "dsp_2", AdvertiserID: "nike", Category: "sportswear", Price: 4.00},
		{DSPID: "dsp_3", AdvertiserID: "bmw", Category: "automotive", Price: 3.00},
	}

	eligible := auction.FilterBidsWithSeparation(bids, sc)
	if len(eligible) != 2 {
		t.Fatalf("expected 2 eligible (Pepsi blocked), got %d", len(eligible))
	}
	if eligible[0].AdvertiserID != "nike" || eligible[1].AdvertiserID != "bmw" {
		t.Error("expected nike and bmw to pass separation filter")
	}
}
