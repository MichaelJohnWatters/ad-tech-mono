// Package cache provides a layered caching system:
//
//	L1: In-process (Go sync.Map) - zero latency, per-instance
//	L2: Redis - shared across instances, atomic operations
//	L3: Postgres - source of truth (via pkg/store/postgres)
//
// Cache invalidation via NATS pub/sub (fire-and-forget).
// Budget handling uses Redis DECRBY with NATS replay for recovery.
//
// Usage:
//
//	c := cache.New(redisClient, logger)
//	c.L1.Set("campaign:123", campaignData, 5*time.Minute)
//	val, ok := c.L1.Get("campaign:123")
//	c.L2.Set(ctx, "dsp:budget:camp_123", "1000.00", 0)
package cache

import (
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// Cache provides L1 (in-process) and L2 (Redis) caching.
type Cache struct {
	L1  *L1Cache
	L2  L2Cache
	log *slog.Logger
}

// New creates a Cache with L1 in-process and L2 Redis layers.
func New(l2 L2Cache, clk clock.Clock, log *slog.Logger) *Cache {
	return &Cache{
		L1:  NewL1(clk),
		L2:  l2,
		log: log,
	}
}

// L1Cache is an in-process cache using sync.Map with TTL support.
// Fastest possible access - zero network latency.
// Per-instance: each pod has its own L1.
// Invalidated via NATS pub/sub.
type L1Cache struct {
	data sync.Map
	clk  clock.Clock
}

type l1Entry struct {
	value     any
	expiresAt time.Time
}

// NewL1 creates an L1 in-process cache.
func NewL1(clk clock.Clock) *L1Cache {
	return &L1Cache{clk: clk}
}

// Get retrieves a value from L1. Returns (value, true) if found and not expired.
func (c *L1Cache) Get(key string) (any, bool) {
	raw, ok := c.data.Load(key)
	if !ok {
		return nil, false
	}
	entry := raw.(*l1Entry)
	if !entry.expiresAt.IsZero() && c.clk.Now().After(entry.expiresAt) {
		c.data.Delete(key)
		return nil, false
	}
	return entry.value, true
}

// Set stores a value in L1 with an optional TTL.
// Pass 0 for no expiry.
func (c *L1Cache) Set(key string, value any, ttl time.Duration) {
	entry := &l1Entry{value: value}
	if ttl > 0 {
		entry.expiresAt = c.clk.Now().Add(ttl)
	}
	c.data.Store(key, entry)
}

// Delete removes a key from L1.
func (c *L1Cache) Delete(key string) {
	c.data.Delete(key)
}

// Clear removes all entries from L1.
// Used when receiving a cache invalidation event.
func (c *L1Cache) Clear() {
	c.data.Range(func(key, _ any) bool {
		c.data.Delete(key)
		return true
	})
}

// ClearPrefix removes all keys matching a prefix from L1.
func (c *L1Cache) ClearPrefix(prefix string) {
	c.data.Range(func(key, _ any) bool {
		if k, ok := key.(string); ok && len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			c.data.Delete(key)
		}
		return true
	})
}
