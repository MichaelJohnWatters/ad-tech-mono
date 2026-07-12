package main

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

// microsPerUSD is the fixed-point scale for the spend counter: 1 USD =
// 1,000,000 micro-dollars. Micros (not cents) because a realized per-impression
// cost is sub-cent — a $5.00 CPM books $0.005 = 5,000 µ, which cents would
// truncate to zero. Must match pkg/billing's pacing accumulator so the
// committed-spend snapshot reconciles onto this counter in the same unit.
const microsPerUSD = 1_000_000

// BudgetTracker is the Redis-backed daily spend ledger.
//
// Spend is stored as fixed-point micro-dollars (price × 1_000_000) in an INT64
// counter so DECRBY/INCRBY remain atomic across pods.
//
// The key is stamped with the UTC calendar day (dsp:budget:{yyyy-mm-dd}:{cid}:
// spent) so the budget resets at UTC midnight — matching the billing engine's
// committed accumulator (pkg/billing pacing), which also rolls on the UTC day.
// This keeps the reconcile source (committed, UTC-day) and the counter it
// overwrites on the SAME day boundary; a plain rolling TTL would have let the
// two disagree at midnight. ttlFn (dsp.budget_reset_interval, ~24h) is now just
// the cleanup TTL that expires yesterday's key; the reset itself is the day
// rollover in the key name.
type BudgetTracker struct {
	l2    cache.L2Cache
	ttlFn func() time.Duration
	nowFn func() time.Time
	log   *slog.Logger
}

// NewBudgetTracker wires the tracker to an L2 cache (Redis in prod, in-memory in tests).
func NewBudgetTracker(l2 cache.L2Cache, ttlFn func() time.Duration, log *slog.Logger) *BudgetTracker {
	return &BudgetTracker{l2: l2, ttlFn: ttlFn, nowFn: time.Now, log: log}
}

// dateFmt is the UTC day stamp shared with pkg/billing's accumulator (dayKey).
const dateFmt = "2006-01-02"

func budgetKey(day, campaignID string) string {
	return "dsp:budget:" + day + ":" + campaignID + ":spent"
}

func (b *BudgetTracker) today() string { return b.nowFn().UTC().Format(dateFmt) }

// Spend returns today's spend for a campaign in major units (dollars).
func (b *BudgetTracker) Spend(campaignID string) float64 {
	v, ok, err := b.l2.Get(context.Background(), budgetKey(b.today(), campaignID))
	if err != nil {
		b.log.Warn("budget read failed", "campaign", campaignID, "error", err)
		return 0
	}
	if !ok {
		return 0
	}
	micros, _ := strconv.ParseInt(v, 10, 64)
	return float64(micros) / microsPerUSD
}

// Record adds amount (in major units) to today's spend for a campaign.
// First write creates the key with the current TTL; subsequent writes just
// atomically increment.
func (b *BudgetTracker) Record(campaignID string, amount float64) {
	ctx := context.Background()
	key := budgetKey(b.today(), campaignID)
	micros := int64(math.Round(amount * microsPerUSD))

	// Set the TTL on the first write of the day; INCRBY preserves it after.
	if _, ok, _ := b.l2.Get(ctx, key); !ok {
		if err := b.l2.Set(ctx, key, "0", b.ttlFn()); err != nil {
			b.log.Warn("budget initial set failed", "campaign", campaignID, "error", err)
			return
		}
	}

	if _, err := b.l2.IncrBy(ctx, key, micros); err != nil {
		b.log.Warn("budget incr failed", "campaign", campaignID, "error", err)
	}
}

// Reconcile overwrites a campaign's spend counter to an authoritative value in
// micro-dollars, sourced from the billing engine's committed-spend snapshot. The local
// Record path (win notice) is a fast, conservative over-count — it counts every
// win, including phantom wins that never impress and the full clearing price on
// CPC/CPA where only the settle bills. This Set corrects the counter to what
// actually bills, so between snapshots pacing stays overspend-safe (local
// over-count) and on each snapshot it snaps to billed reality.
//
// Refreshes the daily TTL so the reconciled value expires with the budget
// window like a Record-written counter would.
//
// day is the snapshot's UTC day (the day the committed value is FOR), so the
// Set lands on the same key Spend reads for that day. An empty day falls back
// to the tracker's today (defensive — publishers always stamp it).
func (b *BudgetTracker) Reconcile(day, campaignID string, micros int64) {
	if micros < 0 {
		micros = 0
	}
	if day == "" {
		day = b.today()
	}
	if err := b.l2.Set(context.Background(), budgetKey(day, campaignID), strconv.FormatInt(micros, 10), b.ttlFn()); err != nil {
		b.log.Warn("budget reconcile failed", "campaign", campaignID, "error", err)
	}
}
