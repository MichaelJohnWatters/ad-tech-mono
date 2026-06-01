package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

func TestFreqCap_AllowAndRecord(t *testing.T) {
	l2 := cache.NewMemoryL2()
	log := slog.New(slog.NewTextHandler(nopWriter{}, nil))
	fc := NewFreqCap(l2, 3, time.Hour, log)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if !fc.AllowAndRecord(ctx, "u1", "c1") {
			t.Fatalf("impression %d should be allowed", i)
		}
	}
	if fc.AllowAndRecord(ctx, "u1", "c1") {
		t.Fatal("4th impression should be blocked")
	}
}

func TestFreqCap_NoUserBypass(t *testing.T) {
	fc := NewFreqCap(cache.NewMemoryL2(), 1, time.Hour, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if !fc.AllowAndRecord(ctx, "", "c1") {
			t.Fatal("empty user id should always allow")
		}
	}
}

func TestFreqCap_PerCampaignIndependent(t *testing.T) {
	fc := NewFreqCap(cache.NewMemoryL2(), 1, time.Hour, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	if !fc.AllowAndRecord(ctx, "u1", "c1") {
		t.Fatal("c1 should be allowed")
	}
	if !fc.AllowAndRecord(ctx, "u1", "c2") {
		t.Fatal("c2 cap must be independent of c1")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
