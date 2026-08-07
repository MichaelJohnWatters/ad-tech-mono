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

// capScopeIDs is the whole campaign-scope knob: it must pick the household
// counter as the (single) enforcement key for household-scoped campaigns, and
// fall back to the untouched default pair otherwise.
func TestCapScopeIDs(t *testing.T) {
	cases := []struct {
		name, scope, user, hh string
		wantUID, wantHH       string
		wantHHPrimary         bool
	}{
		{"default scope keeps both", "", "u1", "hh:1", "u1", "hh:1", false},
		{"explicit user scope keeps both", "user", "u1", "hh:1", "u1", "hh:1", false},
		{"household scope swaps to hh key only", "household", "u1", "hh:1", "hh:1", "", true},
		{"household scope no hh id falls back to user", "household", "u1", "", "u1", "", false},
		{"unknown scope behaves as default", "banana", "u1", "hh:1", "u1", "hh:1", false},
		{"household scope, hh only (no user id)", "household", "", "hh:1", "hh:1", "", true},
	}
	for _, tc := range cases {
		uid, hhid, hhPrimary := capScopeIDs(tc.scope, tc.user, tc.hh)
		if uid != tc.wantUID || hhid != tc.wantHH || hhPrimary != tc.wantHHPrimary {
			t.Errorf("%s: capScopeIDs(%q,%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.name, tc.scope, tc.user, tc.hh, uid, hhid, hhPrimary, tc.wantUID, tc.wantHH, tc.wantHHPrimary)
		}
	}
}

// Household-scoped enforcement: the campaign limit binds on the HOUSEHOLD
// counter (co-viewers share it), the per-user counter is never touched, and
// the blocked scope is attributed to "household" even though the hh id rode
// in the primary slot.
func TestFreqCap_ScopedHousehold(t *testing.T) {
	l2 := cache.NewMemoryL2()
	fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	const hh = "hh:feedbeef"

	// Two different users in one household share the allowance of 2.
	if ok, scope := fc.ScopedDecideAndRecord(ctx, "household", "u1", hh, "c1", 2, time.Hour); !ok || scope != "" {
		t.Fatalf("serve 1: ok=%v scope=%q, want allowed", ok, scope)
	}
	if ok, scope := fc.ScopedDecideAndRecord(ctx, "household", "u2", hh, "c1", 2, time.Hour); !ok || scope != "" {
		t.Fatalf("serve 2: ok=%v scope=%q, want allowed", ok, scope)
	}
	if ok, scope := fc.ScopedDecideAndRecord(ctx, "household", "u3", hh, "c1", 2, time.Hour); ok || scope != "household" {
		t.Fatalf("serve 3: ok=%v scope=%q, want blocked at household", ok, scope)
	}
	// The household counter carries the count; no per-user counter was touched.
	if v, _, _ := l2.Get(ctx, freqCapKey(hh, "c1")); v != "3" {
		t.Fatalf("household counter = %q, want 3", v)
	}
	for _, u := range []string{"u1", "u2", "u3"} {
		if _, present, _ := l2.Get(ctx, freqCapKey(u, "c1")); present {
			t.Fatalf("user counter %s must not exist under household scope", u)
		}
	}
}

// Household scope with NO resolvable household id falls back to the per-user
// counter — mirroring how the platform household leg vanishes on an absent
// hh: id — and attributes a block to "user".
func TestFreqCap_ScopedHouseholdFallbackToUser(t *testing.T) {
	l2 := cache.NewMemoryL2()
	fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()

	if ok, _ := fc.ScopedDecideAndRecord(ctx, "household", "u1", "", "c1", 1, time.Hour); !ok {
		t.Fatal("fallback serve 1 should be allowed")
	}
	if ok, scope := fc.ScopedDecideAndRecord(ctx, "household", "u1", "", "c1", 1, time.Hour); ok || scope != "user" {
		t.Fatalf("fallback serve 2: ok=%v scope=%q, want blocked at user", ok, scope)
	}
	if v, _, _ := l2.Get(ctx, freqCapKey("u1", "c1")); v != "2" {
		t.Fatalf("user counter = %q, want 2", v)
	}
}

// Default (user) scope through the Scoped wrappers is byte-for-byte the
// pre-knob behaviour: user primary, household co-enforced.
func TestFreqCap_ScopedDefaultUnchanged(t *testing.T) {
	l2 := cache.NewMemoryL2()
	fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()

	if ok, scope := fc.ScopedDecideAndRecord(ctx, "", "u1", "hh:1", "c1", 1, time.Hour); !ok || scope != "" {
		t.Fatalf("serve 1: ok=%v scope=%q, want allowed", ok, scope)
	}
	if ok, scope := fc.ScopedDecideAndRecord(ctx, "", "u1", "hh:1", "c1", 1, time.Hour); ok || scope != "user" {
		t.Fatalf("serve 2: ok=%v scope=%q, want blocked at user", ok, scope)
	}
	// Both counters exist: user carries both attempts, household only the
	// allowed one (the pre-existing asymmetry).
	if v, _, _ := l2.Get(ctx, freqCapKey("u1", "c1")); v != "2" {
		t.Fatalf("user counter = %q, want 2", v)
	}
	if v, _, _ := l2.Get(ctx, freqCapKey("hh:1", "c1")); v != "1" {
		t.Fatalf("household counter = %q, want 1", v)
	}
}

// The video/audio PEEK/RECORD split under household scope: peeks never
// increment, records land on the household key, and the cap binds after
// `limit` records across the whole household.
func TestFreqCap_ScopedPeekRecordHousehold(t *testing.T) {
	l2 := cache.NewMemoryL2()
	fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	const hh = "hh:ctvhouse"

	for i := 0; i < 10; i++ {
		if ok, _ := fc.ScopedPeek(ctx, "household", "u1", hh, "c1", 2); !ok {
			t.Fatalf("peek %d should be allowed (nothing recorded)", i)
		}
	}
	if _, present, _ := l2.Get(ctx, freqCapKey(hh, "c1")); present {
		t.Fatal("peek must not increment the household counter")
	}
	fc.ScopedRecord(ctx, "household", "u1", hh, "c1", 2, time.Hour)
	fc.ScopedRecord(ctx, "household", "u2", hh, "c1", 2, time.Hour)
	if ok, scope := fc.ScopedPeek(ctx, "household", "u3", hh, "c1", 2); ok || scope != "household" {
		t.Fatalf("peek after 2 records: ok=%v scope=%q, want blocked at household", ok, scope)
	}
	if _, present, _ := l2.Get(ctx, freqCapKey("u1", "c1")); present {
		t.Fatal("record must not touch the per-user counter under household scope")
	}
}

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
