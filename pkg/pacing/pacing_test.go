package pacing_test

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pacing"
)

func TestTargetSpend_Even(t *testing.T) {
	midnight := time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)

	pacer := pacing.New(clk, pacing.Config{
		Mode:        pacing.ModeEven,
		DailyBudget: 100.0,
		DayStartUTC: midnight,
	})

	// At midnight: 0%
	target := pacer.TargetSpend()
	if target != 0 {
		t.Errorf("at midnight, target = %f, want 0", target)
	}

	// At 6am (25% of day): $25
	clk.Advance(6 * time.Hour)
	target = pacer.TargetSpend()
	if target < 24.9 || target > 25.1 {
		t.Errorf("at 6am, target = %f, want ~25", target)
	}

	// At 3pm (62.5%): $62.50
	clk.Advance(9 * time.Hour)
	target = pacer.TargetSpend()
	if target < 62.4 || target > 62.6 {
		t.Errorf("at 3pm, target = %f, want ~62.5", target)
	}

	// At midnight (100%): $100
	clk.Advance(9 * time.Hour)
	target = pacer.TargetSpend()
	if target < 99.9 || target > 100.1 {
		t.Errorf("at end of day, target = %f, want ~100", target)
	}
}

func TestTargetSpend_FrontLoaded(t *testing.T) {
	midnight := time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)

	pacer := pacing.New(clk, pacing.Config{
		Mode:        pacing.ModeFrontLoaded,
		DailyBudget: 100.0,
		DayStartUTC: midnight,
	})

	// At noon (50% of day): should have spent 80%
	clk.Advance(12 * time.Hour)
	target := pacer.TargetSpend()
	if target < 79.9 || target > 80.1 {
		t.Errorf("front-loaded at noon, target = %f, want ~80", target)
	}

	// At 6pm (75% of day): should have spent 90%
	clk.Advance(6 * time.Hour)
	target = pacer.TargetSpend()
	if target < 89.9 || target > 90.1 {
		t.Errorf("front-loaded at 6pm, target = %f, want ~90", target)
	}
}

func TestShouldBid_ASAP(t *testing.T) {
	clk := clock.NewFake(time.Now())
	pacer := pacing.New(clk, pacing.Config{
		Mode:        pacing.ModeASAP,
		DailyBudget: 100.0,
	})

	// Under budget: always bid
	if !pacer.ShouldBid(50.0) {
		t.Error("ASAP should always bid when under budget")
	}

	// Over budget: stop
	if pacer.ShouldBid(101.0) {
		t.Error("ASAP should stop bidding when over budget")
	}
}

func TestPacingRatio(t *testing.T) {
	midnight := time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)

	pacer := pacing.New(clk, pacing.Config{
		Mode:        pacing.ModeEven,
		DailyBudget: 100.0,
		DayStartUTC: midnight,
	})

	// At noon: target $50
	clk.Advance(12 * time.Hour)

	// Spent exactly target: ratio = 1.0
	ratio := pacer.PacingRatio(50.0)
	if ratio < 0.99 || ratio > 1.01 {
		t.Errorf("on pace ratio = %f, want ~1.0", ratio)
	}

	// Behind pace: ratio < 1.0
	ratio = pacer.PacingRatio(30.0)
	if ratio >= 1.0 {
		t.Errorf("behind pace ratio = %f, should be < 1.0", ratio)
	}

	// Ahead of pace: ratio > 1.0
	ratio = pacer.PacingRatio(70.0)
	if ratio <= 1.0 {
		t.Errorf("ahead of pace ratio = %f, should be > 1.0", ratio)
	}
}

func TestShouldBid_Even_BehindPace(t *testing.T) {
	midnight := time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)

	pacer := pacing.New(clk, pacing.Config{
		Mode:        pacing.ModeEven,
		DailyBudget: 100.0,
		DayStartUTC: midnight,
	})

	// At noon, target is $50. Only spent $20 (way behind).
	clk.Advance(12 * time.Hour)

	// Should almost always bid when behind pace
	bidCount := 0
	for i := 0; i < 100; i++ {
		if pacer.ShouldBid(20.0) {
			bidCount++
		}
	}
	// Behind pace (ratio 0.4): should bid on ~100% of requests
	if bidCount < 95 {
		t.Errorf("behind pace: bid %d/100 times, expected ~100", bidCount)
	}
}
