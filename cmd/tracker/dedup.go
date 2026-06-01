package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
)

// Dedup is the SetNX-backed event dedup gate.
//
// A pixel can fire twice for the same trace (browser retry, double tap,
// network blip). The first request wins; subsequent ones for the same
// (event_type, trace_id) are silently dropped so we don't double-count
// in the analytics store.
type Dedup struct {
	l2      cache.L2Cache
	ttl     time.Duration
	enabled bool
	log     *slog.Logger
}

func NewDedup(l2 cache.L2Cache, ttl time.Duration, enabled bool, log *slog.Logger) *Dedup {
	return &Dedup{l2: l2, ttl: ttl, enabled: enabled, log: log}
}

// FirstSeen returns true if this is the first time we've seen (eventType, traceID).
// Returns true on cache failure (fail-open) so events are never silently dropped.
func (d *Dedup) FirstSeen(ctx context.Context, eventType, traceID string) bool {
	if !d.enabled || traceID == "" {
		return true
	}
	key := "tracker:seen:" + eventType + ":" + traceID
	ok, err := d.l2.SetNX(ctx, key, "1", d.ttl)
	if err != nil {
		d.log.Warn("dedup setnx failed", "key", key, "error", err)
		return true
	}
	return ok
}

// connectRedis returns a real Redis L2 cache if reachable, else MemoryL2.
func connectRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := cfg.Get("redis.url", "localhost:6379")
	pwd := cfg.Get("redis.password", "")
	db := cfg.GetInt("redis.db", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db})
	if err != nil {
		log.Warn("redis unreachable, falling back to in-memory L2", "addr", addr, "error", err)
		return cache.NewMemoryL2()
	}
	log.Info("redis connected", "addr", addr)
	return client
}
