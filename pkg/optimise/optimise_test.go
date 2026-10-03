package optimise

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/bidshading"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func TestBidOptimiser_Overpaying(t *testing.T) {
	tracker := bidshading.NewTracker()
	// Simulate overpaying: win rate > 80%
	for i := 0; i < 90; i++ {
		tracker.RecordWin("pl-1", "adv-1", 5.00, 3.00)
	}
	for i := 0; i < 10; i++ {
		tracker.RecordLoss("pl-1", "adv-1", 4.00, 6.00, bidshading.ReasonOutbid)
	}

	log := logger.New("test")
	opt := NewBidOptimiser(tracker, log)
	adjustments := opt.Run()

	found := false
	for _, a := range adjustments {
		if a.PlacementID == "pl-1" && a.SuggestedBid < a.CurrentBid {
			found = true
		}
	}
	if !found {
		t.Error("expected overpaying adjustment for pl-1")
	}
}

func TestBidOptimiser_Underbidding(t *testing.T) {
	tracker := bidshading.NewTracker()
	// Win rate < 20%
	for i := 0; i < 15; i++ {
		tracker.RecordWin("pl-2", "adv-2", 2.00, 1.80)
	}
	for i := 0; i < 85; i++ {
		tracker.RecordLoss("pl-2", "adv-2", 2.00, 4.00, bidshading.ReasonOutbid)
	}

	log := logger.New("test")
	opt := NewBidOptimiser(tracker, log)
	adjustments := opt.Run()

	found := false
	for _, a := range adjustments {
		if a.PlacementID == "pl-2" {
			found = true
		}
	}
	if !found {
		t.Error("expected underbidding adjustment for pl-2")
	}
}

func TestScorePlacements(t *testing.T) {
	tracker := bidshading.NewTracker()
	for i := 0; i < 50; i++ {
		tracker.RecordWin("pl-good", "adv-3", 2.00, 1.50)
	}
	for i := 0; i < 50; i++ {
		tracker.RecordLoss("pl-bad", "adv-3", 1.00, 3.00, bidshading.ReasonOutbid)
	}

	scores := ScorePlacements(tracker)
	if len(scores) != 2 {
		t.Fatalf("expected 2 scores, got %d", len(scores))
	}
}

func TestBandit_Selection(t *testing.T) {
	b := NewBandit([]string{"cr-1", "cr-2", "cr-3"})

	// cr-2 performs best
	for i := 0; i < 100; i++ {
		b.RecordImpression("cr-1")
		b.RecordImpression("cr-2")
		b.RecordImpression("cr-3")
	}
	for i := 0; i < 5; i++ {
		b.RecordClick("cr-1")
	}
	for i := 0; i < 20; i++ {
		b.RecordClick("cr-2")
	}
	for i := 0; i < 2; i++ {
		b.RecordClick("cr-3")
	}

	// After training, cr-2 should be selected most often
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		counts[b.Select()]++
	}

	if counts["cr-2"] < counts["cr-1"] {
		t.Errorf("cr-2 should be selected more than cr-1: cr-1=%d cr-2=%d", counts["cr-1"], counts["cr-2"])
	}
	if counts["cr-2"] < counts["cr-3"] {
		t.Errorf("cr-2 should be selected more than cr-3: cr-2=%d cr-3=%d", counts["cr-2"], counts["cr-3"])
	}
}

func TestBandit_Weights(t *testing.T) {
	b := NewBandit([]string{"a", "b"})
	for i := 0; i < 50; i++ {
		b.RecordImpression("a")
		b.RecordImpression("b")
	}
	for i := 0; i < 15; i++ {
		b.RecordClick("b") // b is better
	}
	for i := 0; i < 3; i++ {
		b.RecordClick("a")
	}

	weights := b.Weights()
	if weights["b"] < weights["a"] {
		t.Errorf("b should have higher weight: a=%.2f b=%.2f", weights["a"], weights["b"])
	}
}

func TestRebalanceBudgets(t *testing.T) {
	items := []LineItemPerformance{
		{ID: "li-1", Impressions: 1000, Clicks: 20, Spend: 100}, // CTR 2%
		{ID: "li-2", Impressions: 1000, Clicks: 5, Spend: 100},  // CTR 0.5%
		{ID: "li-3", Impressions: 1000, Clicks: 10, Spend: 100}, // CTR 1%
	}

	result := RebalanceBudgets("io-1", items, 300, DefaultReallocationConfig())
	if result == nil {
		t.Fatal("expected reallocation")
	}
	if len(result.Reallocations) != 3 {
		t.Fatalf("expected 3 reallocations, got %d", len(result.Reallocations))
	}

	// li-1 (best CTR) should get increase, li-2 (worst) should get decrease
	var li1Change, li2Change float64
	for _, r := range result.Reallocations {
		switch r.LineItemID {
		case "li-1":
			li1Change = r.ChangePct
		case "li-2":
			li2Change = r.ChangePct
		}
	}
	if li1Change <= 0 {
		t.Errorf("li-1 (best) should increase, got %.1f%%", li1Change)
	}
	if li2Change >= 0 {
		t.Errorf("li-2 (worst) should decrease, got %.1f%%", li2Change)
	}
}

func TestGenerateRecommendations(t *testing.T) {
	// Overpaying campaign
	recs := GenerateRecommendations(CampaignMetrics{
		CampaignID: "c1", Impressions: 10000, Clicks: 100,
		WinRate: 0.92, AvgCPM: 5.00, CTR: 1.0,
		Spend: 900, Budget: 1000, DaysRemaining: 10,
	})

	found := false
	for _, r := range recs {
		if r.Type == "bid_adjustment" {
			found = true
		}
	}
	if !found {
		t.Error("expected bid_adjustment recommendation for overpaying campaign")
	}
}

func TestSmartRouter_Selection(t *testing.T) {
	router := NewSmartRouter()
	const ch = "display"

	// DSP A: high bid rate, fast
	for i := 0; i < 100; i++ {
		router.RecordCall(ch, "dsp-a", true, 3.00, 10*time.Millisecond, false)
	}
	// DSP B: low bid rate
	for i := 0; i < 100; i++ {
		router.RecordCall(ch, "dsp-b", i < 3, 1.00, 20*time.Millisecond, false)
	}
	// DSP C: high timeout rate
	for i := 0; i < 100; i++ {
		router.RecordCall(ch, "dsp-c", false, 0, 200*time.Millisecond, i > 40)
	}

	selected := router.SelectDSPs(ch, []string{"dsp-a", "dsp-b", "dsp-c"})

	// dsp-a should be first (best performer)
	if len(selected) == 0 || selected[0] != "dsp-a" {
		t.Errorf("expected dsp-a first, got %v", selected)
	}

	// dsp-b should be excluded (< 5% bid rate)
	for _, id := range selected {
		if id == "dsp-b" {
			t.Error("dsp-b should be excluded (low bid rate)")
		}
	}

	// dsp-c should be excluded (> 50% timeout)
	for _, id := range selected {
		if id == "dsp-c" {
			t.Error("dsp-c should be excluded (high timeout rate)")
		}
	}
}

// TestSmartRouter_PerChannelStats — a DSP that bids prolifically on display
// but never on video should still be selected for display auctions. Without
// per-channel segmentation, mixing the two would let the video no-bid rate
// drag the display selection.
func TestSmartRouter_PerChannelStats(t *testing.T) {
	router := NewSmartRouter()

	// display: 100% bid rate
	for i := 0; i < 100; i++ {
		router.RecordCall("display", "dsp-a", true, 2.50, 5*time.Millisecond, false)
	}
	// video: 0% bid rate (over the 5% skip threshold after 20 calls)
	for i := 0; i < 50; i++ {
		router.RecordCall("video", "dsp-a", false, 0, 5*time.Millisecond, false)
	}

	displaySelection := router.SelectDSPs("display", []string{"dsp-a"})
	if len(displaySelection) != 1 || displaySelection[0] != "dsp-a" {
		t.Errorf("display: expected dsp-a, got %v — channel stats mixed?", displaySelection)
	}

	videoSelection := router.SelectDSPs("video", []string{"dsp-a"})
	if len(videoSelection) != 0 {
		t.Errorf("video: expected dsp-a excluded (0%% bid rate on video), got %v", videoSelection)
	}

	// Sanity: both stat rows exist independently
	stats := router.Stats()
	if len(stats) != 2 {
		t.Errorf("Stats() returned %d rows, want 2 (one per channel)", len(stats))
	}
}
