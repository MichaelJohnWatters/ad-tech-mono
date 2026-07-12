package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

func TestBudgetTracker_RecordAndSpend(t *testing.T) {
	l2 := cache.NewMemoryL2()
	b := NewBudgetTracker(l2, func() time.Duration { return time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	b.Record("camp-1", 2.50)
	b.Record("camp-1", 0.75)
	if got := b.Spend("camp-1"); got != 3.25 {
		t.Errorf("Spend = %f, want 3.25", got)
	}
}

func TestBudgetTracker_IndependentCampaigns(t *testing.T) {
	b := NewBudgetTracker(cache.NewMemoryL2(), func() time.Duration { return time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	b.Record("camp-1", 1.00)
	b.Record("camp-2", 2.00)
	if b.Spend("camp-1") != 1.00 {
		t.Errorf("camp-1 spend = %f, want 1.00", b.Spend("camp-1"))
	}
	if b.Spend("camp-2") != 2.00 {
		t.Errorf("camp-2 spend = %f, want 2.00", b.Spend("camp-2"))
	}
}

func TestBudgetTracker_ZeroForUnknownCampaign(t *testing.T) {
	b := NewBudgetTracker(cache.NewMemoryL2(), func() time.Duration { return time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	if got := b.Spend("never-seen"); got != 0 {
		t.Errorf("Spend = %f, want 0", got)
	}
}

func TestBudgetTracker_ReconcileOverwrites(t *testing.T) {
	b := NewBudgetTracker(cache.NewMemoryL2(), func() time.Duration { return time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	// Local win-notice over-count (e.g. phantom wins + full CPC clearing price).
	b.Record("camp-1", 5.00)
	if got := b.Spend("camp-1"); got != 5.00 {
		t.Fatalf("pre-reconcile Spend = %f, want 5.00", got)
	}

	// Billing snapshot says only $2.00 actually committed — reconcile down.
	// Snapshot values are micro-dollars: $2.00 = 2,000,000 µ.
	b.Reconcile("", "camp-1", 2_000_000)
	if got := b.Spend("camp-1"); got != 2.00 {
		t.Fatalf("post-reconcile Spend = %f, want 2.00", got)
	}

	// A subsequent local win increments from the reconciled baseline, not the
	// stale over-count.
	b.Record("camp-1", 1.00)
	if got := b.Spend("camp-1"); got != 3.00 {
		t.Fatalf("post-record Spend = %f, want 3.00", got)
	}
}

func TestBudgetTracker_ReconcileClampsNegative(t *testing.T) {
	b := NewBudgetTracker(cache.NewMemoryL2(), func() time.Duration { return time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	b.Reconcile("", "camp-1", -100)
	if got := b.Spend("camp-1"); got != 0 {
		t.Fatalf("Spend = %f, want 0", got)
	}
}

// Budget keys are stamped with the UTC day so the DSP budget resets at UTC
// midnight, matching the billing accumulator. Spend on a new day sees a fresh
// counter, and a reconcile for one day doesn't leak into another.
func TestBudgetTracker_UTCDayIsolation(t *testing.T) {
	b := NewBudgetTracker(cache.NewMemoryL2(), func() time.Duration { return 48 * time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	day1 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	b.nowFn = func() time.Time { return day1 }

	b.Record("camp-1", 4.00)
	if got := b.Spend("camp-1"); got != 4.00 {
		t.Fatalf("day1 spend = %f, want 4.00", got)
	}

	// Reconcile explicitly targets day1's key regardless of "now".
	// Micro-dollars: $2.50 = 2,500,000 µ.
	b.Reconcile("2026-07-06", "camp-1", 2_500_000)
	if got := b.Spend("camp-1"); got != 2.50 {
		t.Fatalf("after reconcile day1 spend = %f, want 2.50", got)
	}

	// Advance to the next UTC day: the counter is fresh (budget reset at midnight).
	b.nowFn = func() time.Time { return day1.Add(24 * time.Hour) }
	if got := b.Spend("camp-1"); got != 0 {
		t.Fatalf("day2 spend = %f, want 0 (rolled at UTC midnight)", got)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
