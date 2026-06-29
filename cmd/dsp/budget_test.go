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

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
