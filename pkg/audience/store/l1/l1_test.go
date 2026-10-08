package l1

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// countingLookup records how many times the real lookup was invoked, so tests
// can prove a cache HIT does NOT call through to Redis.
type countingLookup struct {
	mu    sync.Mutex
	calls map[string]int
	priv  []string
	pub   []string
	err   error
}

func newCounting() *countingLookup { return &countingLookup{calls: map[string]int{}} }

func (f *countingLookup) SegmentsForUser(_ context.Context, u string) ([]string, error) {
	f.bump("pub:" + u)
	return f.pub, f.err
}
func (f *countingLookup) DSPSegmentsForUser(_ context.Context, u string) ([]string, error) {
	f.bump("priv:" + u)
	return f.priv, f.err
}
func (f *countingLookup) bump(k string) {
	f.mu.Lock()
	f.calls[k]++
	f.mu.Unlock()
}
func (f *countingLookup) n(k string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[k]
}

func newCache(inner *countingLookup, enabled *bool, ttl time.Duration) *Cache {
	return Wrap(inner, func() bool { return *enabled }, func() time.Duration { return ttl }, nil)
}

func TestCache_HitServesFromMemory(t *testing.T) {
	en := true
	inner := newCounting()
	inner.priv = []string{"segB"}
	c := newCache(inner, &en, time.Minute)
	defer c.Stop()
	ctx := context.Background()

	g1, _ := c.DSPSegmentsForUser(ctx, "u1") // miss → inner called
	g2, _ := c.DSPSegmentsForUser(ctx, "u1") // hit → inner NOT called

	if got := inner.n("priv:u1"); got != 1 {
		t.Fatalf("want exactly 1 inner call (2nd served from cache), got %d", got)
	}
	if len(g1) != 1 || len(g2) != 1 || g1[0] != "segB" || g2[0] != "segB" {
		t.Fatalf("cache returned wrong segments: %v / %v", g1, g2)
	}
}

func TestCache_AbsoluteTTL_HitDoesNotExtend(t *testing.T) {
	en := true
	inner := newCounting()
	inner.priv = []string{"s"}
	c := newCache(inner, &en, 30*time.Millisecond)
	defer c.Stop()
	ctx := context.Background()

	c.DSPSegmentsForUser(ctx, "u1") // fetch 1 (miss) — sets expiry at +30ms
	time.Sleep(15 * time.Millisecond)
	c.DSPSegmentsForUser(ctx, "u1") // hit — must NOT push expiry out
	if got := inner.n("priv:u1"); got != 1 {
		t.Fatalf("mid-TTL lookup should be a cache hit, inner calls=%d", got)
	}
	time.Sleep(20 * time.Millisecond) // total 35ms > 30ms TTL from fetch 1
	c.DSPSegmentsForUser(ctx, "u1")   // expired (absolute) → re-fetch
	if got := inner.n("priv:u1"); got != 2 {
		t.Fatalf("absolute TTL should have expired → re-fetch; inner calls=%d (hit wrongly extended TTL)", got)
	}
}

func TestCache_DisabledIsPassthrough(t *testing.T) {
	en := false
	inner := newCounting()
	inner.priv = []string{"s"}
	c := newCache(inner, &en, time.Minute)
	defer c.Stop()
	ctx := context.Background()

	c.DSPSegmentsForUser(ctx, "u1")
	c.DSPSegmentsForUser(ctx, "u1")
	if got := inner.n("priv:u1"); got != 2 {
		t.Fatalf("disabled cache must pass through every call, inner calls=%d", got)
	}
}

func TestCache_NegativeCachesEmpty(t *testing.T) {
	en := true
	inner := newCounting() // priv is nil → "no segments"
	c := newCache(inner, &en, time.Minute)
	defer c.Stop()
	ctx := context.Background()

	g1, _ := c.DSPSegmentsForUser(ctx, "nobody") // miss
	g2, _ := c.DSPSegmentsForUser(ctx, "nobody") // hit on the empty result
	if got := inner.n("priv:nobody"); got != 1 {
		t.Fatalf("empty result should be negative-cached, inner calls=%d", got)
	}
	if len(g1) != 0 || len(g2) != 0 {
		t.Fatalf("want empty segments, got %v / %v", g1, g2)
	}
}

func TestCache_CopyIsolation(t *testing.T) {
	en := true
	inner := newCounting()
	inner.pub = []string{"a", "b"}
	c := newCache(inner, &en, time.Minute)
	defer c.Stop()
	ctx := context.Background()

	c.SegmentsForUser(ctx, "u1")             // warm
	hit, _ := c.SegmentsForUser(ctx, "u1")   // hit → returns a copy
	hit[0] = "MUTATED"                       // caller mutates its copy
	again, _ := c.SegmentsForUser(ctx, "u1") // next hit must be untouched
	if again[0] != "a" {
		t.Fatalf("mutating a returned slice corrupted the cache: %v", again)
	}
}

func TestCache_ErrorNotCached(t *testing.T) {
	en := true
	inner := newCounting()
	inner.err = errors.New("redis down")
	c := newCache(inner, &en, time.Minute)
	defer c.Stop()
	ctx := context.Background()

	c.DSPSegmentsForUser(ctx, "u1") // miss, errors — must not cache
	inner.err = nil
	inner.priv = []string{"x"}
	g, _ := c.DSPSegmentsForUser(ctx, "u1") // must re-query (error wasn't cached)
	if got := inner.n("priv:u1"); got != 2 {
		t.Fatalf("errored lookup must not be cached, inner calls=%d", got)
	}
	if len(g) != 1 || g[0] != "x" {
		t.Fatalf("want recovered segments, got %v", g)
	}
}

// BenchmarkCacheHit measures the hot win path: a cache hit (no Redis), which
// costs a shard lock + a small slice copy.
func BenchmarkCacheHit(b *testing.B) {
	en := true
	inner := newCounting()
	inner.priv = []string{"seg1", "seg2"}
	c := newCache(inner, &en, time.Minute)
	defer c.Stop()
	ctx := context.Background()
	c.DSPSegmentsForUser(ctx, "u1") // warm
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = c.DSPSegmentsForUser(ctx, "u1")
		}
	})
}
