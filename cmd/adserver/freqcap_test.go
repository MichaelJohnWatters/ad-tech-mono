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
	fc := NewFreqCap(l2, log)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if !fc.AllowAndRecord(ctx, "u1", "c1", 3, time.Hour) {
			t.Fatalf("impression %d should be allowed", i)
		}
	}
	if fc.AllowAndRecord(ctx, "u1", "c1", 3, time.Hour) {
		t.Fatal("4th impression should be blocked")
	}
}

func TestFreqCap_NoUserBypass(t *testing.T) {
	fc := NewFreqCap(cache.NewMemoryL2(), slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if !fc.AllowAndRecord(ctx, "", "c1", 1, time.Hour) {
			t.Fatal("empty user id should always allow")
		}
	}
}

func TestFreqCap_PerCampaignIndependent(t *testing.T) {
	fc := NewFreqCap(cache.NewMemoryL2(), slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	if !fc.AllowAndRecord(ctx, "u1", "c1", 1, time.Hour) {
		t.Fatal("c1 should be allowed")
	}
	if !fc.AllowAndRecord(ctx, "u1", "c2", 1, time.Hour) {
		t.Fatal("c2 cap must be independent of c1")
	}
}

// Household capping is the same counter keyed by the hh: id — the serve
// handler calls AllowAndRecord twice (user key, then household key). This
// exercises the property that matters: two different users sharing one
// household id exhaust ONE shared cap.
func TestFreqCap_HouseholdSharedAcrossUsers(t *testing.T) {
	fc := NewFreqCap(cache.NewMemoryL2(), slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	const hh = "hh:abcd1234efgh5678"

	// Viewer 1 on the CTV: allowed, consumes household slot 1 of 2.
	if !fc.AllowAndRecord(ctx, hh, "c1", 2, time.Hour) {
		t.Fatal("household impression 1 should be allowed")
	}
	// Viewer 2 on a phone, same household: allowed, consumes slot 2.
	if !fc.AllowAndRecord(ctx, hh, "c1", 2, time.Hour) {
		t.Fatal("household impression 2 should be allowed")
	}
	// Any device in the household is now capped.
	if fc.AllowAndRecord(ctx, hh, "c1", 2, time.Hour) {
		t.Fatal("household impression 3 should be blocked (shared cap)")
	}
	// A different household is unaffected.
	if !fc.AllowAndRecord(ctx, "hh:9999999999999999", "c1", 2, time.Hour) {
		t.Fatal("other household must have its own counter")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
