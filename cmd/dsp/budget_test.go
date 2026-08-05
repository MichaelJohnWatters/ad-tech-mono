package main

import (
	"context"
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

// countingL2 counts read ops so tests can pin the bid-path rule: nothing in
// the per-campaign loop may do per-call network I/O.
type countingL2 struct {
	cache.L2Cache
	gets int
}

func (c *countingL2) Get(ctx context.Context, key string) (string, bool, error) {
	c.gets++
	return c.L2Cache.Get(ctx, key)
}

// The bid path (Spend) must NEVER touch Redis — reads come from the in-process
// copy kept warm by RefreshSpend (background) and this pod's own writes.
func TestBudgetTracker_SpendNeverTouchesRedis(t *testing.T) {
	l2 := &countingL2{L2Cache: cache.NewMemoryL2()}
	b := NewBudgetTracker(l2, func() time.Duration { return time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	b.Record("camp-1", 2.50)
	l2.gets = 0
	for i := 0; i < 100; i++ {
		if got := b.Spend("camp-1"); got != 2.50 {
			t.Fatalf("Spend = %f, want 2.50", got)
		}
		b.Spend("camp-never-refreshed") // unknown key must also stay off Redis
	}
	if l2.gets != 0 {
		t.Fatalf("bid-path Spend did %d Redis GETs, want 0", l2.gets)
	}
}

// RefreshSpend (the background bulk refresher) makes counters written by OTHER
// pods visible, and prunes keys that left the refresh set (day rollover /
// deleted campaigns).
func TestBudgetTracker_RefreshSpendBulk(t *testing.T) {
	l2 := cache.NewMemoryL2()
	b := NewBudgetTracker(l2, func() time.Duration { return time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	// A sibling pod recorded spend straight into Redis.
	other := NewBudgetTracker(l2, func() time.Duration { return time.Hour }, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	other.Record("camp-1", 3.00)

	if got := b.Spend("camp-1"); got != 0 {
		t.Fatalf("pre-refresh Spend = %f, want 0 (not yet visible)", got)
	}
	b.RefreshSpend(context.Background(), []string{"camp-1", "camp-2"})
	if got := b.Spend("camp-1"); got != 3.00 {
		t.Fatalf("post-refresh Spend = %f, want 3.00", got)
	}
	if got := b.Spend("camp-2"); got != 0 {
		t.Fatalf("absent counter Spend = %f, want 0", got)
	}

	// camp-1 leaves the catalog → its entry is pruned, reads go back to 0.
	b.RefreshSpend(context.Background(), []string{"camp-2"})
	if got := b.Spend("camp-1"); got != 0 {
		t.Fatalf("pruned Spend = %f, want 0", got)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
