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

// TestFlightAware_PacesOverFlightWindow verifies flight-aware pacing spreads the
// LIFETIME budget over the flight window (not the daily budget over 24h). This is
// what makes a short-flight campaign pace visibly over minutes: at 25% through a
// 1-hour flight, ~25% of the lifetime budget should be the target.
func TestFlightAware_PacesOverFlightWindow(t *testing.T) {
	start := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	pacer := pacing.New(clk, pacing.Config{
		Mode:           pacing.ModeEven,
		DailyBudget:    600, // daily cap; not the pace basis in flight mode
		DayStartUTC:    start.Truncate(24 * time.Hour),
		FlightStart:    start,
		FlightEnd:      start.Add(1 * time.Hour), // 1-hour flight
		LifetimeBudget: 600,
	})

	// Start of flight: 0.
	if got := pacer.TargetSpend(); got != 0 {
		t.Errorf("flight start target = %f, want 0", got)
	}
	// 15 min into a 1-hour flight = 25% → ~$150 (NOT ~$0.10 that 24h daily pacing
	// would give — this is the whole point of flight-aware pacing).
	clk.Advance(15 * time.Minute)
	if got := pacer.TargetSpend(); got < 149 || got > 151 {
		t.Errorf("15m/1h flight target = %f, want ~150 (25%% of lifetime)", got)
	}
	// 30 min = 50% → ~$300.
	clk.Advance(15 * time.Minute)
	if got := pacer.TargetSpend(); got < 299 || got > 301 {
		t.Errorf("30m/1h flight target = %f, want ~300", got)
	}
	// Past flight end → capped at lifetime budget.
	clk.Advance(2 * time.Hour)
	if got := pacer.TargetSpend(); got < 599 || got > 601 {
		t.Errorf("post-flight target = %f, want ~600 (capped)", got)
	}
}

// TestFlightAware_BackwardCompatible verifies that with no flight set, pacing is
// identical to the legacy daily behaviour.
func TestFlightAware_BackwardCompatible(t *testing.T) {
	midnight := time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)
	pacer := pacing.New(clk, pacing.Config{
		Mode:        pacing.ModeEven,
		DailyBudget: 100,
		DayStartUTC: midnight,
		// no flight fields
	})
	clk.Advance(6 * time.Hour) // 25% of the day
	if got := pacer.TargetSpend(); got < 24.9 || got > 25.1 {
		t.Errorf("legacy daily target at 6am = %f, want ~25", got)
	}
}

// TestFlightAware_ASAPUsesLifetime verifies ASAP in flight mode bids until the
// lifetime budget is spent, not the daily budget.
func TestFlightAware_ASAPUsesLifetime(t *testing.T) {
	start := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	pacer := pacing.New(clk, pacing.Config{
		Mode:           pacing.ModeASAP,
		DailyBudget:    100,
		FlightStart:    start,
		FlightEnd:      start.Add(1 * time.Hour),
		LifetimeBudget: 500,
	})
	if !pacer.ShouldBid(300) { // over daily(100) but under lifetime(500)
		t.Error("ASAP flight: should bid at 300 spend (under 500 lifetime)")
	}
	if pacer.ShouldBid(500) { // at lifetime → stop
		t.Error("ASAP flight: should NOT bid at 500 spend (== lifetime)")
	}
}
