package bidshading

import (
	"math"
	"testing"
)

func TestTracker_WinLoss(t *testing.T) {
	tracker := NewTracker()

	tracker.RecordWin("pl-1", "", 3.00, 2.80)
	tracker.RecordWin("pl-1", "", 2.50, 2.30)
	tracker.RecordLoss("pl-1", "", 2.00, 3.00, ReasonOutbid)
	tracker.RecordLoss("pl-1", "", 1.50, 3.00, ReasonOutbid)
	tracker.RecordLoss("pl-1", "", 0.50, 1.00, ReasonBelowFloor)

	stats := tracker.Stats("pl-1")
	if stats.TotalBids != 5 {
		t.Errorf("total = %d, want 5", stats.TotalBids)
	}
	if stats.Wins != 2 {
		t.Errorf("wins = %d, want 2", stats.Wins)
	}
	if stats.Losses != 3 {
		t.Errorf("losses = %d, want 3", stats.Losses)
	}
	if stats.Outbid != 2 {
		t.Errorf("outbid = %d, want 2", stats.Outbid)
	}
	if stats.BelowFloor != 1 {
		t.Errorf("below_floor = %d, want 1", stats.BelowFloor)
	}
	expectedWinRate := 2.0 / 5.0
	if math.Abs(stats.WinRate-expectedWinRate) > 0.01 {
		t.Errorf("win_rate = %.2f, want %.2f", stats.WinRate, expectedWinRate)
	}
}

func TestTracker_EmptyPlacement(t *testing.T) {
	tracker := NewTracker()
	stats := tracker.Stats("nonexistent")
	if stats.TotalBids != 0 {
		t.Error("expected 0 bids for nonexistent placement")
	}
}

func TestTracker_WinRateCurve(t *testing.T) {
	tracker := NewTracker()

	// Simulate: low bids lose, high bids win
	for i := 0; i < 50; i++ {
		tracker.RecordLoss("pl-1", "", 1.00, 3.00, ReasonOutbid)
	}
	for i := 0; i < 30; i++ {
		tracker.RecordLoss("pl-1", "", 2.00, 3.00, ReasonOutbid)
	}
	for i := 0; i < 20; i++ {
		tracker.RecordWin("pl-1", "", 2.00, 1.80)
	}
	for i := 0; i < 40; i++ {
		tracker.RecordWin("pl-1", "", 3.00, 2.50)
	}
	for i := 0; i < 10; i++ {
		tracker.RecordLoss("pl-1", "", 3.00, 4.00, ReasonOutbid)
	}

	curve := tracker.WinRateCurve("pl-1")
	if len(curve.Points) == 0 {
		t.Fatal("expected curve points")
	}
	if curve.Midpoint <= 0 {
		t.Errorf("midpoint = %.2f, want > 0", curve.Midpoint)
	}

	// Should be able to get a bid for 60% win rate
	bid60 := curve.BidForWinRate(0.60)
	if bid60 <= 0 {
		t.Errorf("bid for 60%% win rate = %.2f, want > 0", bid60)
	}
	t.Logf("curve: midpoint=%.2f, bid_for_60%%=%.2f, points=%d", curve.Midpoint, bid60, len(curve.Points))
}

func TestCurve_BidForWinRate_EdgeCases(t *testing.T) {
	// Empty curve
	c := Curve{}
	if c.BidForWinRate(0.5) != 0 {
		t.Error("expected 0 for empty curve")
	}

	// Zero target
	c = Curve{Midpoint: 2.0, Points: []CurvePoint{{BidPrice: 2.0, WinRate: 0.5, Samples: 10}}}
	if c.BidForWinRate(0) != 0 {
		t.Error("expected 0 for zero target")
	}
}

func TestTracker_AllPlacements(t *testing.T) {
	tracker := NewTracker()
	tracker.RecordWin("pl-1", "", 2.0, 1.8)
	tracker.RecordWin("pl-2", "", 3.0, 2.5)
	tracker.RecordLoss("pl-3", "", 1.0, 2.0, ReasonOutbid)

	placements := tracker.AllPlacements()
	if len(placements) != 3 {
		t.Errorf("expected 3 placements, got %d", len(placements))
	}
}

// TestPerAdvertiserTally locks phase-2: a reporting-only per-(placement,advertiser)
// tally that never changes the pooled view that drives shading.
func TestPerAdvertiserTally(t *testing.T) {
	tr := NewTracker()
	tr.RecordWin("pl-1", "adv-A", 2.00, 1.80)
	tr.RecordWin("pl-1", "adv-A", 2.50, 2.30)
	tr.RecordLoss("pl-1", "adv-A", 1.00, 3.00, ReasonOutbid)
	tr.RecordWin("pl-1", "adv-B", 3.00, 2.90)
	tr.RecordLoss("pl-2", "adv-A", 1.50, 4.00, ReasonOutbid)

	// Pooled view aggregates BOTH advertisers on pl-1 (this drives shading).
	if got := tr.Stats("pl-1").TotalBids; got != 4 {
		t.Errorf("pooled pl-1 TotalBids = %d, want 4", got)
	}
	// Per-advertiser: adv-A's OWN pl-1 = 2 wins + 1 loss.
	if a := tr.StatsFor("pl-1", "adv-A"); a.TotalBids != 3 || a.Wins != 2 || a.Losses != 1 {
		t.Errorf("adv-A pl-1 = %+v, want 3 bids / 2 wins / 1 loss", a)
	}
	// adv-B's OWN pl-1 is independent of adv-A.
	if b := tr.StatsFor("pl-1", "adv-B"); b.TotalBids != 1 || b.Wins != 1 {
		t.Errorf("adv-B pl-1 = %+v, want 1 bid / 1 win", b)
	}
	// AdvertiserStats: adv-A competed on pl-1 and pl-2 only.
	all := tr.AdvertiserStats("adv-A")
	if len(all) != 2 || all["pl-1"].Wins != 2 || all["pl-2"].Losses != 1 {
		t.Errorf("adv-A stats = %+v, want pl-1(2 wins) + pl-2(1 loss)", all)
	}
	if len(tr.AdvertiserStats("adv-none")) != 0 {
		t.Error("adv-none should have no placements")
	}
	// Empty advertiser id records pooled only — never a byAdv entry.
	tr.RecordWin("pl-9", "", 1.0, 1.0)
	if len(tr.AdvertiserStats("")) != 0 {
		t.Error(`"" advertiser must not create a byAdv entry`)
	}
}
