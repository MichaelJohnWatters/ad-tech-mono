package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
)

// Dedup is the SetNX-backed event dedup gate.
//
// A pixel can fire twice for the same trace (browser retry, double tap,
// network blip). The first request wins; subsequent ones for the same
// (event_type, trace_id) are silently dropped so we don't double-count
// in the analytics store.
type Dedup struct {
	l2        cache.L2Cache
	ttlFn     func() time.Duration
	enabledFn func() bool
	log       *slog.Logger
}

// ttlFn and enabledFn are called per request so live edits to
// tracker.dedup_ttl / tracker.dedup_enabled take effect on the next pixel.
func NewDedup(l2 cache.L2Cache, ttlFn func() time.Duration, enabledFn func() bool, log *slog.Logger) *Dedup {
	return &Dedup{l2: l2, ttlFn: ttlFn, enabledFn: enabledFn, log: log}
}

// FirstSeen returns true if this is the first time we've seen (eventType, traceID).
// Returns true on cache failure (fail-open) so events are never silently dropped.
func (d *Dedup) FirstSeen(ctx context.Context, eventType, traceID string) bool {
	if !d.enabledFn() || traceID == "" {
		return true
	}
	key := "tracker:seen:" + eventType + ":" + traceID
	ok, err := d.l2.SetNX(ctx, key, "1", d.ttlFn())
	if err != nil {
		d.log.Warn("dedup setnx failed", "key", key, "error", err)
		return true
	}
	return ok
}

// connectRedis returns a real Redis L2 cache if reachable, else MemoryL2.
func connectRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := keys.Redis.URL.Get(cfg)
	pwd := keys.Redis.Password.Get(cfg)
	db := keys.Redis.DB.Get(cfg)
	// Self-healing: a failed boot dial no longer latches MemoryL2 forever —
	// the wrapper serves fail-open from memory and swaps to Redis when the
	// background retry lands (pkg/cache/selfheal.go).
	return cache.NewSelfHealingL2(func(ctx context.Context) (cache.L2Cache, error) {
		return cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db})
	}, 10*time.Second, addr, log)
}
