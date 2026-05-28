package clock_test

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

func TestRealClock(t *testing.T) {
	clk := clock.Real{}

	before := time.Now()
	now := clk.Now()
	after := time.Now()

	if now.Before(before) || now.After(after) {
		t.Errorf("Real.Now() returned %v, expected between %v and %v", now, before, after)
	}

	since := clk.Since(before)
	if since < 0 {
		t.Errorf("Real.Since() returned negative duration: %v", since)
	}

	future := time.Now().Add(time.Hour)
	until := clk.Until(future)
	if until < 0 {
		t.Errorf("Real.Until() returned negative duration for future time: %v", until)
	}
}

func TestFakeClock_Now(t *testing.T) {
	fixed := time.Date(2026, 5, 28, 15, 0, 0, 0, time.UTC)
	clk := clock.NewFake(fixed)

	if got := clk.Now(); !got.Equal(fixed) {
		t.Errorf("Fake.Now() = %v, want %v", got, fixed)
	}
}

func TestFakeClock_Advance(t *testing.T) {
	start := time.Date(2026, 5, 28, 15, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)

	clk.Advance(3 * time.Hour)

	expected := time.Date(2026, 5, 28, 18, 0, 0, 0, time.UTC)
	if got := clk.Now(); !got.Equal(expected) {
		t.Errorf("After Advance(3h), Now() = %v, want %v", got, expected)
	}
}

func TestFakeClock_Set(t *testing.T) {
	clk := clock.NewFake(time.Now())

	newTime := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clk.Set(newTime)

	if got := clk.Now(); !got.Equal(newTime) {
		t.Errorf("After Set(), Now() = %v, want %v", got, newTime)
	}
}

func TestFakeClock_Since(t *testing.T) {
	now := time.Date(2026, 5, 28, 15, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)

	past := now.Add(-2 * time.Hour)
	since := clk.Since(past)

	if since != 2*time.Hour {
		t.Errorf("Fake.Since() = %v, want 2h", since)
	}
}

func TestFakeClock_Until(t *testing.T) {
	now := time.Date(2026, 5, 28, 15, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)

	future := now.Add(5 * time.Hour)
	until := clk.Until(future)

	if until != 5*time.Hour {
		t.Errorf("Fake.Until() = %v, want 5h", until)
	}
}

func TestFakeClock_After(t *testing.T) {
	now := time.Date(2026, 5, 28, 15, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)

	ch := clk.After(time.Hour)

	select {
	case got := <-ch:
		if !got.Equal(now) {
			t.Errorf("Fake.After() sent %v, want %v", got, now)
		}
	case <-time.After(time.Second):
		t.Error("Fake.After() did not return immediately")
	}
}

// TestFakeClock_Pacing demonstrates using the fake clock
// to test budget pacing logic without waiting real time.
func TestFakeClock_Pacing(t *testing.T) {
	// Simulate: campaign starts at midnight, $100/day budget
	midnight := time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)

	dailyBudget := 100.0
	daySeconds := 24.0 * 60 * 60

	// At midnight: should have spent 0%
	elapsed := clk.Since(midnight).Seconds()
	expectedSpend := dailyBudget * (elapsed / daySeconds)
	if expectedSpend != 0 {
		t.Errorf("At midnight, expected spend = 0, got %f", expectedSpend)
	}

	// Advance to 3pm (62.5% of day)
	clk.Advance(15 * time.Hour)
	elapsed = clk.Since(midnight).Seconds()
	expectedSpend = dailyBudget * (elapsed / daySeconds)

	// Allow small floating point tolerance
	if expectedSpend < 62.4 || expectedSpend > 62.6 {
		t.Errorf("At 3pm, expected spend ~62.5, got %f", expectedSpend)
	}

	// Advance to 6pm (75% of day)
	clk.Advance(3 * time.Hour)
	elapsed = clk.Since(midnight).Seconds()
	expectedSpend = dailyBudget * (elapsed / daySeconds)

	if expectedSpend < 74.9 || expectedSpend > 75.1 {
		t.Errorf("At 6pm, expected spend ~75.0, got %f", expectedSpend)
	}
}

// TestFakeClock_AttributionWindow demonstrates testing a 7-day
// attribution window without waiting 7 real days.
func TestFakeClock_AttributionWindow(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC))

	impressionTime := clk.Now()
	attributionWindow := 7 * 24 * time.Hour

	// 3 days later: within window
	clk.Advance(3 * 24 * time.Hour)
	if clk.Since(impressionTime) > attributionWindow {
		t.Error("3 days should be within 7-day window")
	}

	// 8 days later: outside window
	clk.Advance(5 * 24 * time.Hour)
	if clk.Since(impressionTime) <= attributionWindow {
		t.Error("8 days should be outside 7-day window")
	}
}

// Verify both types satisfy the Clock interface at compile time.
var _ clock.Clock = clock.Real{}
var _ clock.Clock = &clock.Fake{}
