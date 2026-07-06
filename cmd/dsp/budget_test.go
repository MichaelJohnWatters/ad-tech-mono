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
	b.Reconcile("camp-1", 200)
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
	b.Reconcile("camp-1", -100)
	if got := b.Spend("camp-1"); got != 0 {
		t.Fatalf("Spend = %f, want 0", got)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
