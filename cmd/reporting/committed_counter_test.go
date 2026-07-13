package main

import (
	"context"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeCommittedRedis is an in-memory stand-in for the Redis client, exercising
// the redisCommittedCounter's key/set logic without a real server.
type fakeCommittedRedis struct {
	mu   sync.Mutex
	ints map[string]int64
	sets map[string]map[string]bool
}

func newFakeCommittedRedis() *fakeCommittedRedis {
	return &fakeCommittedRedis{ints: map[string]int64{}, sets: map[string]map[string]bool{}}
}

func (f *fakeCommittedRedis) IncrBy(_ context.Context, key string, n int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ints[key] += n
	return f.ints[key], nil
}

func (f *fakeCommittedRedis) Get(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.ints[key]
	if !ok {
		return "", false, nil
	}
	return strconv.FormatInt(v, 10), true, nil
}

func (f *fakeCommittedRedis) Set(_ context.Context, key, value string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, _ := strconv.ParseInt(value, 10, 64)
	f.ints[key] = n
	return nil
}

func (f *fakeCommittedRedis) SAdd(_ context.Context, key, member string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sets[key] == nil {
		f.sets[key] = map[string]bool{}
	}
	f.sets[key][member] = true
	return nil
}

func (f *fakeCommittedRedis) SMembers(_ context.Context, key string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sets[key]))
	for m := range f.sets[key] {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeCommittedRedis) Expire(_ context.Context, _ string, _ time.Duration) error { return nil }

// TestRedisCommittedCounter_AdditiveAndReconcile exercises the deployable
// counter: INCRBY-based deltas from separate calls (as separate replicas would
// issue) sum commutatively, negative deltas lower the total, and Reconcile
// overwrites only the campaigns it names.
func TestRedisCommittedCounter_AdditiveAndReconcile(t *testing.T) {
	c := newRedisCommittedCounter(newFakeCommittedRedis(), time.Hour)
	ctx := context.Background()
	day := "2026-07-13"

	// Two replicas contribute to camp-a; camp-b from one only.
	if err := c.AddDelta(ctx, day, map[string]int64{"camp-a": 4000, "camp-b": 6000}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddDelta(ctx, day, map[string]int64{"camp-a": 5000}); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Snapshot(ctx, day)
	if want := (map[string]int64{"camp-a": 9000, "camp-b": 6000}); !reflect.DeepEqual(got, want) {
		t.Fatalf("additive snapshot = %v, want %v", got, want)
	}

	// A swept reserve lowers a total.
	if err := c.AddDelta(ctx, day, map[string]int64{"camp-b": -1000}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Snapshot(ctx, day)
	if got["camp-b"] != 5000 {
		t.Fatalf("camp-b after negative delta = %d, want 5000", got["camp-b"])
	}

	// Reconcile resets camp-a to authoritative truth; camp-b untouched.
	if err := c.Reconcile(ctx, day, map[string]int64{"camp-a": 20000}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Snapshot(ctx, day)
	if want := (map[string]int64{"camp-a": 20000, "camp-b": 5000}); !reflect.DeepEqual(got, want) {
		t.Fatalf("post-reconcile snapshot = %v, want %v", got, want)
	}
}

// TestRedisCommittedCounter_DayIsolation confirms counters are scoped per UTC
// day, so yesterday's totals never leak into today's snapshot.
func TestRedisCommittedCounter_DayIsolation(t *testing.T) {
	c := newRedisCommittedCounter(newFakeCommittedRedis(), time.Hour)
	ctx := context.Background()
	_ = c.AddDelta(ctx, "2026-07-12", map[string]int64{"camp-a": 100})
	_ = c.AddDelta(ctx, "2026-07-13", map[string]int64{"camp-a": 250})

	today, _ := c.Snapshot(ctx, "2026-07-13")
	if today["camp-a"] != 250 {
		t.Fatalf("today camp-a = %d, want 250 (yesterday must not leak)", today["camp-a"])
	}
}
