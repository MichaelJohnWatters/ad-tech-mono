// Package l1 is the in-process L1 cache for the per-auction audience lookup —
// Phase 1 of the audience cache, built after the Step-0 shadow probe
// (pkg/audience/store/probe) measured strong request locality (~80% would-be
// hits on the DSP dsp_private lookup, ~37% on the SSP public lookup at a 3s TTL).
//
// It wraps an audstore.Lookup and serves a user's segments from memory for a
// short ABSOLUTE TTL, turning the top steady hot-path I/O (a ~46ms Redis
// SMEMBERS, every auction) into a ~0ms map read on a hit. Discipline:
//   - Absolute TTL: a cache HIT serves but does NOT extend the entry's expiry,
//     so even a constantly-active user is re-read from Redis at least every TTL
//     (sliding expiry would starve the hottest users of freshness).
//   - Fall through to the inner Lookup on miss/expiry — worst case == today.
//   - Negative-cache empties (a user with no segments is a valid cached result).
//   - Returns a COPY on a hit, preserving the "fresh slice per call" contract of
//     the Redis reader so no caller can mutate the shared cached slice.
//   - The shard lock is released BEFORE the inner Redis call (never hold a lock
//     across I/O).
//   - Bounded to the live working set by a janitor that prunes expired entries
//     (set size is rps × TTL, tiny), with a hard per-shard backstop.
//
// Staleness bound: a freshly-enrolled retargeting user resolves within
// TTL + the audience changelog drainer window (~3s + ~3s) — within the lag the
// system already tolerates. Live kill-switch via the *_cache_enabled config key.
package l1

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	audstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store"
)

const (
	numShards   = 256
	maxPerShard = 8192 // hard backstop (~2M entries/array); real working set is rps×ttl
	janitorTick = 5 * time.Second
	defaultTTL  = 3 * time.Second
)

type entry struct {
	segs []string
	exp  int64 // absolute expiry, unix millis
}

type shard struct {
	mu sync.Mutex
	m  map[string]entry
}

// Cache decorates an audstore.Lookup with the L1 segment cache. Public and
// dsp_private lookups use separate shard arrays (the map key is just userID).
type Cache struct {
	inner   audstore.Lookup
	enabled func() bool
	ttl     func() time.Duration

	pub  [numShards]shard
	priv [numShards]shard

	hitPublic, hitPrivate   prometheus.Counter
	missPublic, missPrivate prometheus.Counter

	stop chan struct{}
}

// Wrap returns a Cache over inner. enabled/ttl are live-config readers (so the
// cache is a live kill-switch and its TTL is tunable without a restart).
// Counters register on reg (the service's own registry); reg may be nil in tests.
func Wrap(inner audstore.Lookup, enabled func() bool, ttl func() time.Duration, reg prometheus.Registerer) *Cache {
	c := &Cache{inner: inner, enabled: enabled, ttl: ttl, stop: make(chan struct{})}
	for i := range c.pub {
		c.pub[i].m = make(map[string]entry)
		c.priv[i].m = make(map[string]entry)
	}
	hits := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "audience", Name: "l1_cache_hits_total",
		Help: "Audience lookups served from the in-process L1 cache (no Redis call).",
	}, []string{"visibility"})
	misses := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "audience", Name: "l1_cache_misses_total",
		Help: "Audience lookups that missed the L1 cache and fell through to Redis.",
	}, []string{"visibility"})
	if reg != nil {
		reg.MustRegister(hits, misses)
	}
	c.hitPublic, c.hitPrivate = hits.WithLabelValues("public"), hits.WithLabelValues("dsp_private")
	c.missPublic, c.missPrivate = misses.WithLabelValues("public"), misses.WithLabelValues("dsp_private")
	go c.janitor()
	return c
}

// SegmentsForUser serves public segments from L1 (falls through to inner on miss).
func (c *Cache) SegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return c.get(ctx, &c.pub, userID, true, c.inner.SegmentsForUser)
}

// DSPSegmentsForUser serves dsp_private segments from L1 (falls through on miss).
func (c *Cache) DSPSegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return c.get(ctx, &c.priv, userID, false, c.inner.DSPSegmentsForUser)
}

type lookupFn func(context.Context, string) ([]string, error)

func (c *Cache) get(ctx context.Context, shards *[numShards]shard, userID string, public bool, inner lookupFn) ([]string, error) {
	// Disabled or empty key → pure passthrough (the live kill-switch).
	if userID == "" || !c.enabled() {
		return inner(ctx, userID)
	}
	now := time.Now().UnixMilli()
	sh := &shards[shardIndex(userID)]

	sh.mu.Lock()
	e, ok := sh.m[userID]
	if ok && now < e.exp {
		segs := cloneSegs(e.segs) // copy out — callers may append/retain
		sh.mu.Unlock()
		c.incHit(public)
		return segs, nil
	}
	sh.mu.Unlock()

	// Miss/expiry → real lookup WITHOUT holding the shard lock (never block a
	// shard on the ~46ms Redis call).
	segs, err := inner(ctx, userID)
	c.incMiss(public)
	if err != nil {
		return segs, err // never cache an error/degraded result
	}

	stored := cloneSegs(segs) // isolate the stored copy from the returned slice
	ttlMs := c.ttlMillis()
	sh.mu.Lock()
	if _, exists := sh.m[userID]; exists || len(sh.m) < maxPerShard {
		sh.m[userID] = entry{segs: stored, exp: now + ttlMs}
	}
	sh.mu.Unlock()
	return segs, nil
}

func (c *Cache) incHit(public bool) {
	if public {
		c.hitPublic.Inc()
	} else {
		c.hitPrivate.Inc()
	}
}

func (c *Cache) incMiss(public bool) {
	if public {
		c.missPublic.Inc()
	} else {
		c.missPrivate.Inc()
	}
}

func (c *Cache) ttlMillis() int64 {
	ttl := c.ttl()
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return ttl.Milliseconds()
}

func cloneSegs(s []string) []string {
	if len(s) == 0 {
		return nil // negative-cache / empty — nil is the canonical "no segments"
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// shardIndex is an inline, allocation-free FNV-1a over the userID.
func shardIndex(key string) uint32 {
	const prime32 = 16777619
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= prime32
	}
	return h % numShards
}

// janitor prunes expired entries so memory tracks the live working set.
func (c *Cache) janitor() {
	t := time.NewTicker(janitorTick)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			now := time.Now().UnixMilli()
			for i := range c.pub {
				prune(&c.pub[i], now)
				prune(&c.priv[i], now)
			}
		}
	}
}

func prune(sh *shard, now int64) {
	sh.mu.Lock()
	for k, e := range sh.m {
		if now >= e.exp {
			delete(sh.m, k)
		}
	}
	sh.mu.Unlock()
}

// Stop halts the janitor (call on service shutdown).
func (c *Cache) Stop() { close(c.stop) }
