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

// TestFreqCap_PeekAndRecord proves the video split: Allow (peek) never
// increments, so repeated serve decisions / cold-misses don't burn the cap; only
// Record counts. The cap is reached exactly after `limit` Records, matching
// AllowAndRecord's semantics — not after `limit` decisions.
func TestFreqCap_PeekAndRecord(t *testing.T) {
	fc := NewFreqCap(cache.NewMemoryL2(), slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()

	// 100 peeks with zero records: always allowed, nothing burned.
	for i := 0; i < 100; i++ {
		if !fc.Allow(ctx, "u1", "c1", 3) {
			t.Fatalf("peek %d should be allowed (no records yet)", i)
		}
	}
	// Now serve 3 real impressions (peek then record each).
	for i := 1; i <= 3; i++ {
		if !fc.Allow(ctx, "u1", "c1", 3) {
			t.Fatalf("impression %d peek should be allowed", i)
		}
		fc.Record(ctx, "u1", "c1", 3, time.Hour)
	}
	// The cap is now full: the next peek is blocked.
	if fc.Allow(ctx, "u1", "c1", 3) {
		t.Fatal("4th impression peek should be blocked after 3 records")
	}
	// A different campaign is independent.
	if !fc.Allow(ctx, "u1", "c2", 3) {
		t.Fatal("different campaign must not share the counter")
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

// Combined-path semantics (serial fallback on MemoryL2): the household
// counter must NOT be touched when the user scope blocks — the same
// asymmetry the pre-Lua serial path had, preserved by checkScript.
func TestFreqCapDecideAndRecordAsymmetry(t *testing.T) {
	l2 := cache.NewMemoryL2()
	fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	// Cap 1: first serve allowed, second blocked at user.
	if ok, scope := fc.DecideAndRecord(context.Background(), "u1", "hh1", "c1", 1, time.Minute); !ok || scope != "" {
		t.Fatalf("first serve: ok=%v scope=%q, want allowed", ok, scope)
	}
	if ok, scope := fc.DecideAndRecord(context.Background(), "u1", "hh1", "c1", 1, time.Minute); ok || scope != "user" {
		t.Fatalf("second serve: ok=%v scope=%q, want blocked at user", ok, scope)
	}
	// Household counter saw only the ONE allowed serve — a fresh user in the
	// same household still gets an impression under a household cap of 2.
	if v, _, _ := l2.Get(context.Background(), freqCapKey("hh1", "c1")); v != "1" {
		t.Fatalf("household counter = %q, want 1 (not incremented on user-blocked serve)", v)
	}

	// PeekBoth never increments.
	if ok, _ := fc.PeekBoth(context.Background(), "u2", "hh2", "c1", 1); !ok {
		t.Fatal("peek on fresh counters must allow")
	}
	if v, present, _ := l2.Get(context.Background(), freqCapKey("u2", "c1")); present {
		t.Fatalf("peek incremented the counter: %q", v)
	}

	// RecordBoth counts both scopes.
	fc.RecordBoth(context.Background(), "u3", "hh3", "c1", 5, time.Minute)
	if v, _, _ := l2.Get(context.Background(), freqCapKey("hh3", "c1")); v != "1" {
		t.Fatalf("household record = %q, want 1", v)
	}
}
