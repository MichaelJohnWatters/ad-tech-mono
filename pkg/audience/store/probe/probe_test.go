package probe

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeLookup struct{ pub, priv []string }

func (f fakeLookup) SegmentsForUser(context.Context, string) ([]string, error)    { return f.pub, nil }
func (f fakeLookup) DSPSegmentsForUser(context.Context, string) ([]string, error) { return f.priv, nil }

func newTestProbe(enabled *bool, ttl time.Duration) *Probe {
	return Wrap(
		fakeLookup{pub: []string{"segA"}, priv: []string{"segB"}},
		func() bool { return *enabled },
		func() time.Duration { return ttl },
		nil, // nil registry — counters still work for ToFloat64, just unregistered
	)
}

func TestProbe_RecordsHitsAndMisses(t *testing.T) {
	en := true
	p := newTestProbe(&en, time.Minute)
	defer p.Stop()
	ctx := context.Background()

	// Delegation must be unchanged — the inner lookup still runs.
	got, _ := p.SegmentsForUser(ctx, "u1") // first public lookup = miss
	if len(got) != 1 || got[0] != "segA" {
		t.Fatalf("delegation broken: %v", got)
	}
	if n := testutil.ToFloat64(p.missPublic); n != 1 {
		t.Errorf("want 1 public miss after first lookup, got %v", n)
	}

	p.SegmentsForUser(ctx, "u1") // repeat within TTL = hit
	if n := testutil.ToFloat64(p.hitPublic); n != 1 {
		t.Errorf("want 1 public hit on repeat, got %v", n)
	}

	p.SegmentsForUser(ctx, "u2") // new user = miss
	if n := testutil.ToFloat64(p.missPublic); n != 2 {
		t.Errorf("want 2 public misses, got %v", n)
	}

	// dsp_private is tracked independently from public.
	p.DSPSegmentsForUser(ctx, "u1") // first private = miss
	p.DSPSegmentsForUser(ctx, "u1") // repeat private = hit
	if miss, hit := testutil.ToFloat64(p.missPrivate), testutil.ToFloat64(p.hitPrivate); miss != 1 || hit != 1 {
		t.Errorf("private counters wrong: miss=%v hit=%v", miss, hit)
	}
}

func TestProbe_ExpiresAfterTTL(t *testing.T) {
	en := true
	p := newTestProbe(&en, 20*time.Millisecond)
	defer p.Stop()
	ctx := context.Background()

	p.SegmentsForUser(ctx, "u1") // miss (unseen)
	time.Sleep(35 * time.Millisecond)
	p.SegmentsForUser(ctx, "u1") // beyond TTL → miss again, not a hit

	if n := testutil.ToFloat64(p.hitPublic); n != 0 {
		t.Errorf("expected no hits past TTL, got %v", n)
	}
	if n := testutil.ToFloat64(p.missPublic); n != 2 {
		t.Errorf("want 2 misses, got %v", n)
	}
}

func TestProbe_DisabledRecordsNothing(t *testing.T) {
	en := false
	p := newTestProbe(&en, time.Minute)
	defer p.Stop()
	ctx := context.Background()

	got, _ := p.SegmentsForUser(ctx, "u1")
	p.SegmentsForUser(ctx, "u1")
	p.DSPSegmentsForUser(ctx, "u1")

	if len(got) != 1 {
		t.Fatalf("delegation must still work when disabled")
	}
	total := testutil.ToFloat64(p.hitPublic) + testutil.ToFloat64(p.missPublic) +
		testutil.ToFloat64(p.hitPrivate) + testutil.ToFloat64(p.missPrivate)
	if total != 0 {
		t.Errorf("disabled probe recorded %v events (want 0)", total)
	}
}

// BenchmarkProbeDisabled proves the production default (probe off) adds ~nothing
// to the hot path: want 0 allocs/op and ns/op ≈ the bare fake call.
func BenchmarkProbeDisabled(b *testing.B) {
	en := false
	p := newTestProbe(&en, 3*time.Second)
	defer p.Stop()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = p.SegmentsForUser(ctx, "user-x")
		}
	})
}

// BenchmarkProbeEnabled measures the cost when the probe is ON (the measurement
// window only) — a sharded map touch + counter inc over a realistic working set.
func BenchmarkProbeEnabled(b *testing.B) {
	en := true
	p := newTestProbe(&en, 3*time.Second)
	defer p.Stop()
	ctx := context.Background()
	// Pre-size a pool of user ids so we exercise both hits and misses + all shards.
	users := make([]string, 512)
	for i := range users {
		users[i] = "user-" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _ = p.SegmentsForUser(ctx, users[i%len(users)])
			i++
		}
	})
}
