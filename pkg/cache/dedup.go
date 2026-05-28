// Package cache - DedupStore adapter for the events package.
//
// Bridges cache.L2Cache to events.DedupStore interface so the
// idempotent consumer can use Redis for deduplication.
package cache

import (
	"context"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// L2DedupAdapter wraps an L2Cache to implement events.DedupStore.
type L2DedupAdapter struct {
	l2 L2Cache
}

// NewDedupAdapter creates a DedupStore backed by L2 (Redis).
func NewDedupAdapter(l2 L2Cache) events.DedupStore {
	return &L2DedupAdapter{l2: l2}
}

// MarkProcessed uses SetNX to atomically check-and-set.
func (a *L2DedupAdapter) MarkProcessed(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return a.l2.SetNX(ctx, key, "1", ttl)
}

// UnmarkProcessed removes the dedup key (allows retry).
func (a *L2DedupAdapter) UnmarkProcessed(ctx context.Context, key string) error {
	return a.l2.Delete(ctx, key)
}
