package preload

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeL2 is a minimal in-memory cache.L2Cache for the pure prevKeys/tombstone
// tests. Only Get/Set are exercised here; the rest satisfy the interface.
type fakeL2 struct{ m map[string]string }

func newFakeL2() *fakeL2 { return &fakeL2{m: map[string]string{}} }

func (f *fakeL2) Get(_ context.Context, key string) (string, bool, error) {
	v, ok := f.m[key]
	return v, ok, nil
}
func (f *fakeL2) Set(_ context.Context, key, value string, _ time.Duration) error {
	f.m[key] = value
	return nil
}
func (f *fakeL2) Delete(_ context.Context, key string) error { delete(f.m, key); return nil }
func (f *fakeL2) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	if _, ok := f.m[key]; ok {
		return false, nil
	}
	f.m[key] = value
	return true, nil
}
func (f *fakeL2) Incr(context.Context, string) (int64, error)          { return 0, nil }
func (f *fakeL2) IncrBy(context.Context, string, int64) (int64, error) { return 0, nil }
func (f *fakeL2) DecrBy(context.Context, string, int64) (int64, error) { return 0, nil }
func (f *fakeL2) Expire(context.Context, string, time.Duration) error  { return nil }
func (f *fakeL2) SAdd(_ context.Context, key string, members ...string) error {
	cur := map[string]struct{}{}
	if v := f.m[key]; v != "" {
		for _, s := range strings.Split(v, "\n") {
			cur[s] = struct{}{}
		}
	}
	for _, s := range members {
		cur[s] = struct{}{}
	}
	parts := make([]string, 0, len(cur))
	for s := range cur {
		parts = append(parts, s)
	}
	sort.Strings(parts)
	f.m[key] = strings.Join(parts, "\n")
	return nil
}
func (f *fakeL2) SRem(_ context.Context, key string, members ...string) error {
	if f.m[key] == "" {
		return nil
	}
	rm := map[string]struct{}{}
	for _, s := range members {
		rm[s] = struct{}{}
	}
	var parts []string
	for _, s := range strings.Split(f.m[key], "\n") {
		if _, drop := rm[s]; !drop && s != "" {
			parts = append(parts, s)
		}
	}
	f.m[key] = strings.Join(parts, "\n")
	return nil
}
func (f *fakeL2) SMembers(_ context.Context, key string) ([]string, error) {
	if f.m[key] == "" {
		return nil, nil
	}
	return strings.Split(f.m[key], "\n"), nil
}
func (f *fakeL2) ReplaceSet(_ context.Context, key string, members []string, _ time.Duration) error {
	if len(members) == 0 {
		delete(f.m, key)
		return nil
	}
	f.m[key] = strings.Join(members, "\n")
	return nil
}
func (f *fakeL2) Ping(context.Context) error { return nil }
func (f *fakeL2) Close() error               { return nil }

func TestKeysToTombstone(t *testing.T) {
	tests := []struct {
		name          string
		prev, current []string
		want          []string
	}{
		{"member removed is tombstoned", []string{"a", "b"}, []string{"a"}, []string{"b"}},
		{"nothing removed", []string{"a", "b"}, []string{"a", "b"}, nil},
		{"all removed", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"new member only, no tombstone", []string{"a"}, []string{"a", "c"}, nil},
		{"empty prev", nil, []string{"a"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := keysToTombstone(toSet(tc.prev), toSet(tc.current))
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !equalStrings(got, want) {
				t.Errorf("keysToTombstone(%v, %v) = %v, want %v", tc.prev, tc.current, got, want)
			}
		})
	}
}

// TestPrevKeysSharedAcrossPods is the regression guard for the multi-pod bug:
// the "written last cycle" set lives in SHARED Redis, so a DIFFERENT pod (with
// an empty in-memory prevKeys) still learns the keys and can tombstone a
// vanished user. Before the fix, prevKeys was per-pod memory, so a /refresh
// landing on a pod that never wrote the key left the pruned user matching.
func TestPrevKeysSharedAcrossPods(t *testing.T) {
	l2 := newFakeL2()
	podA := &Preloader{l2: l2}
	podB := &Preloader{l2: l2} // fresh pod: prevKeys is nil/empty

	ctx := context.Background()
	// Pod A completes a cycle where userX (public) is a member.
	written := map[string]bool{redisKey("userX", "public"): true}
	podA.savePrevKeys(ctx, written)

	// Pod B, which never saw userX locally, must still recover the prev set
	// from shared Redis.
	prevOnB := podB.loadPrevKeys(ctx)
	if !prevOnB[redisKey("userX", "public")] {
		t.Fatalf("pod B did not learn userX's key from shared Redis: %v", prevOnB)
	}

	// userX is now pruned (current is empty). Pod B computes the tombstone from
	// the shared prev set alone — even with an empty local prevKeys.
	tomb := keysToTombstone(prevOnB, map[string]bool{})
	if len(tomb) != 1 || tomb[0] != redisKey("userX", "public") {
		t.Fatalf("pod B tombstone = %v, want [%s]", tomb, redisKey("userX", "public"))
	}
}

func TestLoadPrevKeysEmptyAndMalformed(t *testing.T) {
	l2 := newFakeL2()
	p := &Preloader{l2: l2}
	ctx := context.Background()

	// Missing key → empty set, no error/panic.
	if got := p.loadPrevKeys(ctx); len(got) != 0 {
		t.Errorf("missing prevkeys = %v, want empty", got)
	}
	// Malformed JSON → empty set (falls back to local + TTL), no panic.
	_ = l2.Set(ctx, prevKeysRedisKey, "{not json", 0)
	if got := p.loadPrevKeys(ctx); len(got) != 0 {
		t.Errorf("malformed prevkeys = %v, want empty", got)
	}
	// Round-trip.
	p.savePrevKeys(ctx, map[string]bool{"k1": true, "k2": true})
	got := p.loadPrevKeys(ctx)
	if !got["k1"] || !got["k2"] || len(got) != 2 {
		t.Errorf("round-trip prevkeys = %v, want {k1,k2}", got)
	}
}

func toSet(keys []string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
