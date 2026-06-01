package main

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

// BudgetTracker is the Redis-backed daily spend ledger.
//
// Spend is stored as fixed-point cents (price × 100) in an INT64 counter
// so DECRBY/INCRBY remain atomic across pods. Each key gets a TTL equal to
// the budget reset interval so daily budgets roll over without a separate job.
type BudgetTracker struct {
	l2  cache.L2Cache
	ttl time.Duration
	log *slog.Logger
}

// NewBudgetTracker wires the tracker to an L2 cache (Redis in prod, in-memory in tests).
func NewBudgetTracker(l2 cache.L2Cache, ttl time.Duration, log *slog.Logger) *BudgetTracker {
	return &BudgetTracker{l2: l2, ttl: ttl, log: log}
}

func budgetKey(campaignID string) string {
	return "dsp:budget:" + campaignID + ":spent"
}

// Spend returns today's spend for a campaign in major units (dollars).
func (b *BudgetTracker) Spend(campaignID string) float64 {
	v, ok, err := b.l2.Get(context.Background(), budgetKey(campaignID))
	if err != nil {
		b.log.Warn("budget read failed", "campaign", campaignID, "error", err)
		return 0
	}
	if !ok {
		return 0
	}
	cents, _ := strconv.ParseInt(v, 10, 64)
	return float64(cents) / 100.0
}

// Record adds amount (in major units) to today's spend for a campaign.
// First write creates the key with TTL; subsequent writes just atomically increment.
func (b *BudgetTracker) Record(campaignID string, amount float64) {
	ctx := context.Background()
	key := budgetKey(campaignID)
	cents := int64(amount * 100)

	// Set the TTL on the first write of the day; INCRBY preserves it after.
	if _, ok, _ := b.l2.Get(ctx, key); !ok {
		if err := b.l2.Set(ctx, key, "0", b.ttl); err != nil {
			b.log.Warn("budget initial set failed", "campaign", campaignID, "error", err)
			return
		}
	}

	if _, err := b.l2.IncrBy(ctx, key, cents); err != nil {
		b.log.Warn("budget incr failed", "campaign", campaignID, "error", err)
	}
}
