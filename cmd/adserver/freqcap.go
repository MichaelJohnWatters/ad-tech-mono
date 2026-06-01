package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

// FreqCap is the Redis-backed per-user-per-campaign impression counter.
//
// AllowAndRecord atomically INCRs the counter and reports whether the
// next impression is allowed. The first INCR also sets the TTL so each
// counter expires after the cap window.
type FreqCap struct {
	l2     cache.L2Cache
	limit  int64
	window time.Duration
	log    *slog.Logger
}

func NewFreqCap(l2 cache.L2Cache, limit int, window time.Duration, log *slog.Logger) *FreqCap {
	return &FreqCap{l2: l2, limit: int64(limit), window: window, log: log}
}

func freqCapKey(userID, campaignID string) string {
	return "adserver:freqcap:" + userID + ":" + campaignID
}

// AllowAndRecord returns true if the impression is under the cap. Increments the
// counter and sets the window TTL on the first increment of a window. Empty
// userID (no consent) bypasses the cap entirely.
func (f *FreqCap) AllowAndRecord(ctx context.Context, userID, campaignID string) bool {
	if userID == "" || f.limit <= 0 {
		return true
	}
	key := freqCapKey(userID, campaignID)
	count, err := f.l2.Incr(ctx, key)
	if err != nil {
		f.log.Warn("freqcap incr failed", "key", key, "error", err)
		return true // fail-open: never block ads on cache failure
	}
	if count == 1 {
		if err := f.l2.Expire(ctx, key, f.window); err != nil {
			f.log.Warn("freqcap expire failed", "key", key, "error", err)
		}
	}
	return count <= f.limit
}
