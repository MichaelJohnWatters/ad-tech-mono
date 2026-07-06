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
// the budget reset interval so daily budgets roll over without a separate
// job.
//
// ttlFn is called on every first-of-day write so a UI edit to
// dsp.budget_reset_interval takes effect on the next campaign's first
// spend of the day, not on next pod restart.
type BudgetTracker struct {
	l2    cache.L2Cache
	ttlFn func() time.Duration
	log   *slog.Logger
}

// NewBudgetTracker wires the tracker to an L2 cache (Redis in prod, in-memory in tests).
func NewBudgetTracker(l2 cache.L2Cache, ttlFn func() time.Duration, log *slog.Logger) *BudgetTracker {
	return &BudgetTracker{l2: l2, ttlFn: ttlFn, log: log}
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
// First write creates the key with the current TTL; subsequent writes just
// atomically increment.
func (b *BudgetTracker) Record(campaignID string, amount float64) {
	ctx := context.Background()
	key := budgetKey(campaignID)
	cents := int64(amount * 100)

	// Set the TTL on the first write of the day; INCRBY preserves it after.
	if _, ok, _ := b.l2.Get(ctx, key); !ok {
		if err := b.l2.Set(ctx, key, "0", b.ttlFn()); err != nil {
			b.log.Warn("budget initial set failed", "campaign", campaignID, "error", err)
			return
		}
	}

	if _, err := b.l2.IncrBy(ctx, key, cents); err != nil {
		b.log.Warn("budget incr failed", "campaign", campaignID, "error", err)
	}
}

// Reconcile overwrites a campaign's spend counter to an authoritative value in
// cents, sourced from the billing engine's committed-spend snapshot. The local
// Record path (win notice) is a fast, conservative over-count — it counts every
// win, including phantom wins that never impress and the full clearing price on
// CPC/CPA where only the settle bills. This Set corrects the counter to what
// actually bills, so between snapshots pacing stays overspend-safe (local
// over-count) and on each snapshot it snaps to billed reality.
//
// Refreshes the daily TTL so the reconciled value expires with the budget
// window like a Record-written counter would.
func (b *BudgetTracker) Reconcile(campaignID string, cents int64) {
	if cents < 0 {
		cents = 0
	}
	if err := b.l2.Set(context.Background(), budgetKey(campaignID), strconv.FormatInt(cents, 10), b.ttlFn()); err != nil {
		b.log.Warn("budget reconcile failed", "campaign", campaignID, "error", err)
	}
}
