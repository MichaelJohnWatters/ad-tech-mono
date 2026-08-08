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

// resolveCapScope is the whole campaign-scope knob: it must resolve the
// household counter as the (single) enforcement leg for household-scoped
// campaigns, and fall back to the untouched default (user, household) pair
// otherwise. The leg carries its own scope label, so blocked-scope
// attribution is fixed at resolution time.
func TestResolveCapScope(t *testing.T) {
	cases := []struct {
		name, scope, user, hh string
		wantLegs              []capLeg
	}{
		{"default scope keeps both", "", "u1", "hh:1", []capLeg{{"user", "u1"}, {"household", "hh:1"}}},
		{"explicit user scope keeps both", "user", "u1", "hh:1", []capLeg{{"user", "u1"}, {"household", "hh:1"}}},
		{"household scope swaps to hh leg only", "household", "u1", "hh:1", []capLeg{{"household", "hh:1"}}},
		{"household scope no hh id falls back to user", "household", "u1", "", []capLeg{{"user", "u1"}}},
		{"unknown scope behaves as default", "banana", "u1", "hh:1", []capLeg{{"user", "u1"}, {"household", "hh:1"}}},
		{"household scope, hh only (no user id)", "household", "", "hh:1", []capLeg{{"household", "hh:1"}}},
		{"default scope, hh only (no user id)", "", "", "hh:1", []capLeg{{"household", "hh:1"}}},
		{"default scope, user only", "", "u1", "", []capLeg{{"user", "u1"}}},
		{"no ids resolves to nothing (cap bypass)", "", "", "", nil},
		{"household scope, no ids resolves to nothing", "household", "", "", nil},
	}
	for _, tc := range cases {
		res := resolveCapScope(tc.scope, tc.user, tc.hh)
		got := res.legs[:res.n]
		if len(got) != len(tc.wantLegs) {
			t.Errorf("%s: resolveCapScope(%q,%q,%q) = %v, want %v", tc.name, tc.scope, tc.user, tc.hh, got, tc.wantLegs)
			continue
		}
		for i := range got {
			if got[i] != tc.wantLegs[i] {
				t.Errorf("%s: leg %d = %+v, want %+v", tc.name, i, got[i], tc.wantLegs[i])
			}
		}
	}
}

// The resolve-once contract, end-to-end against the backing store for every
// (user, household, scope-mode) combo: Peek, RecordAll, and DecideAndRecord
// all consume ONE resolution, so RecordAll creates exactly the resolved
// keys, Peek reads those same keys (blocking once they reach the limit),
// and nothing else in the keyspace is touched. This is the property the old
// Scoped* wrappers could only approximate by re-deriving ids per call.
func TestFreqCap_PeekRecordSameKeysAllCombos(t *testing.T) {
	cases := []struct {
		name, scope, user, hh string
		wantKeys              []string // counter OWNER ids, in leg order
		wantBlocked           string   // scope attributed once every leg is at the limit
	}{
		{"default both ids", "", "u1", "hh:1", []string{"u1", "hh:1"}, "user"},
		{"user scope both ids", "user", "u1", "hh:1", []string{"u1", "hh:1"}, "user"},
		{"household scope both ids", "household", "u1", "hh:1", []string{"hh:1"}, "household"},
		{"household scope no hh id", "household", "u1", "", []string{"u1"}, "user"},
		{"default user only", "", "u1", "", []string{"u1"}, "user"},
		{"default hh only", "", "", "hh:1", []string{"hh:1"}, "household"},
	}
	ctx := context.Background()
	for _, tc := range cases {
		l2 := cache.NewMemoryL2()
		fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
		res := resolveCapScope(tc.scope, tc.user, tc.hh)

		// Resolution is deterministic: the record request of a PEEK/RECORD
		// split (a separate HTTP call re-resolving the same inputs) lands on
		// identical keys.
		again := resolveCapScope(tc.scope, tc.user, tc.hh)
		if res != again {
			t.Errorf("%s: re-resolution differs: %+v vs %+v", tc.name, res, again)
		}

		if ok, _ := fc.Peek(ctx, res, "c1", 1); !ok {
			t.Errorf("%s: fresh peek must allow", tc.name)
		}
		fc.RecordAll(ctx, res, "c1", 1, time.Hour)

		// Exactly the resolved counters exist — no more, no fewer.
		for _, owner := range tc.wantKeys {
			if v, _, _ := l2.Get(ctx, freqCapKey(owner, "c1")); v != "1" {
				t.Errorf("%s: counter for %s = %q, want 1", tc.name, owner, v)
			}
		}
		for _, owner := range []string{"u1", "hh:1"} {
			expected := false
			for _, w := range tc.wantKeys {
				if w == owner {
					expected = true
				}
			}
			if _, present, _ := l2.Get(ctx, freqCapKey(owner, "c1")); present != expected {
				t.Errorf("%s: counter %s present=%v, want %v", tc.name, owner, present, expected)
			}
		}

		// Peek reads the keys RecordAll wrote: at the limit it blocks, and
		// the attribution is the blocking leg's own label.
		if ok, scope := fc.Peek(ctx, res, "c1", 1); ok || scope != tc.wantBlocked {
			t.Errorf("%s: peek at limit: ok=%v scope=%q, want blocked at %q", tc.name, ok, scope, tc.wantBlocked)
		}
	}

	// Empty resolution (no consented ids): everything bypasses, nothing is
	// written, nothing blocks.
	l2 := cache.NewMemoryL2()
	fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	res := resolveCapScope("household", "", "")
	fc.RecordAll(ctx, res, "c1", 1, time.Hour)
	if ok, scope := fc.Peek(ctx, res, "c1", 1); !ok || scope != "" {
		t.Fatalf("empty resolution: ok=%v scope=%q, want bypass", ok, scope)
	}
	if ok, scope := fc.DecideAndRecord(ctx, res, "c1", 1, time.Hour); !ok || scope != "" {
		t.Fatalf("empty resolution decide: ok=%v scope=%q, want bypass", ok, scope)
	}
}

// Household-scoped enforcement: the campaign limit binds on the HOUSEHOLD
// counter (co-viewers share it), the per-user counter is never touched, and
// the blocked scope is attributed to "household" because the household leg
// carries its own label through the resolution.
func TestFreqCap_ScopedHousehold(t *testing.T) {
	l2 := cache.NewMemoryL2()
	fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	const hh = "hh:feedbeef"

	// Two different users in one household share the allowance of 2.
	if ok, scope := fc.DecideAndRecord(ctx, resolveCapScope("household", "u1", hh), "c1", 2, time.Hour); !ok || scope != "" {
		t.Fatalf("serve 1: ok=%v scope=%q, want allowed", ok, scope)
	}
	if ok, scope := fc.DecideAndRecord(ctx, resolveCapScope("household", "u2", hh), "c1", 2, time.Hour); !ok || scope != "" {
		t.Fatalf("serve 2: ok=%v scope=%q, want allowed", ok, scope)
	}
	if ok, scope := fc.DecideAndRecord(ctx, resolveCapScope("household", "u3", hh), "c1", 2, time.Hour); ok || scope != "household" {
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

	legs := resolveCapScope("household", "u1", "")
	if ok, _ := fc.DecideAndRecord(ctx, legs, "c1", 1, time.Hour); !ok {
		t.Fatal("fallback serve 1 should be allowed")
	}
	if ok, scope := fc.DecideAndRecord(ctx, legs, "c1", 1, time.Hour); ok || scope != "user" {
		t.Fatalf("fallback serve 2: ok=%v scope=%q, want blocked at user", ok, scope)
	}
	if v, _, _ := l2.Get(ctx, freqCapKey("u1", "c1")); v != "2" {
		t.Fatalf("user counter = %q, want 2", v)
	}
}

// Default (user) scope through the resolution is byte-for-byte the
// pre-knob behaviour: user primary, household co-enforced.
func TestFreqCap_ScopedDefaultUnchanged(t *testing.T) {
	l2 := cache.NewMemoryL2()
	fc := NewFreqCap(l2, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()

	legs := resolveCapScope("", "u1", "hh:1")
	if ok, scope := fc.DecideAndRecord(ctx, legs, "c1", 1, time.Hour); !ok || scope != "" {
		t.Fatalf("serve 1: ok=%v scope=%q, want allowed", ok, scope)
	}
	if ok, scope := fc.DecideAndRecord(ctx, legs, "c1", 1, time.Hour); ok || scope != "user" {
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
		if ok, _ := fc.Peek(ctx, resolveCapScope("household", "u1", hh), "c1", 2); !ok {
			t.Fatalf("peek %d should be allowed (nothing recorded)", i)
		}
	}
	if _, present, _ := l2.Get(ctx, freqCapKey(hh, "c1")); present {
		t.Fatal("peek must not increment the household counter")
	}
	fc.RecordAll(ctx, resolveCapScope("household", "u1", hh), "c1", 2, time.Hour)
	fc.RecordAll(ctx, resolveCapScope("household", "u2", hh), "c1", 2, time.Hour)
	if ok, scope := fc.Peek(ctx, resolveCapScope("household", "u3", hh), "c1", 2); ok || scope != "household" {
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
	legs := resolveCapScope("", "u1", "hh1")
	if ok, scope := fc.DecideAndRecord(context.Background(), legs, "c1", 1, time.Minute); !ok || scope != "" {
		t.Fatalf("first serve: ok=%v scope=%q, want allowed", ok, scope)
	}
	if ok, scope := fc.DecideAndRecord(context.Background(), legs, "c1", 1, time.Minute); ok || scope != "user" {
		t.Fatalf("second serve: ok=%v scope=%q, want blocked at user", ok, scope)
	}
	// Household counter saw only the ONE allowed serve — a fresh user in the
	// same household still gets an impression under a household cap of 2.
	if v, _, _ := l2.Get(context.Background(), freqCapKey("hh1", "c1")); v != "1" {
		t.Fatalf("household counter = %q, want 1 (not incremented on user-blocked serve)", v)
	}

	// Peek never increments.
	if ok, _ := fc.Peek(context.Background(), resolveCapScope("", "u2", "hh2"), "c1", 1); !ok {
		t.Fatal("peek on fresh counters must allow")
	}
	if v, present, _ := l2.Get(context.Background(), freqCapKey("u2", "c1")); present {
		t.Fatalf("peek incremented the counter: %q", v)
	}

	// RecordAll counts both legs.
	fc.RecordAll(context.Background(), resolveCapScope("", "u3", "hh3"), "c1", 5, time.Minute)
	if v, _, _ := l2.Get(context.Background(), freqCapKey("hh3", "c1")); v != "1" {
		t.Fatalf("household record = %q, want 1", v)
	}
}

// Default-scope block on the HOUSEHOLD leg: a fresh user in a saturated
// household is allowed at user but blocked at household, and the attribution
// names the household leg (the resolution carries the label; nothing remaps
// it after the fact).
func TestFreqCap_DefaultScopeHouseholdBlockAttribution(t *testing.T) {
	fc := NewFreqCap(cache.NewMemoryL2(), slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	ctx := context.Background()
	const hh = "hh:full"

	// Two users exhaust the shared household allowance of 2.
	if ok, _ := fc.DecideAndRecord(ctx, resolveCapScope("", "u1", hh), "c1", 2, time.Hour); !ok {
		t.Fatal("serve 1 should be allowed")
	}
	if ok, _ := fc.DecideAndRecord(ctx, resolveCapScope("", "u2", hh), "c1", 2, time.Hour); !ok {
		t.Fatal("serve 2 should be allowed")
	}
	// u3 is fresh (user count 0) but the household is full.
	if ok, scope := fc.DecideAndRecord(ctx, resolveCapScope("", "u3", hh), "c1", 2, time.Hour); ok || scope != "household" {
		t.Fatalf("serve 3: ok=%v scope=%q, want blocked at household", ok, scope)
	}
	// Same shape through Peek (video/audio path).
	if ok, scope := fc.Peek(ctx, resolveCapScope("", "u4", hh), "c1", 2); ok || scope != "household" {
		t.Fatalf("peek: ok=%v scope=%q, want blocked at household", ok, scope)
	}
}
