package main

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"sync"
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

	// spendCache is the in-process read copy of the Redis counters. The bid
	// handler calls Spend for EVERY eligible campaign on EVERY bid — with a
	// big-world catalog (73 campaigns) that was ~70 sequential Redis round
	// trips per display bid, ~500ms under load, and the exchange's bid
	// deadline fired (2026-08-05: display bid rate 16% vs video 97%, blank
	// no_bid_reason = DeadlineExceeded). The first fix was a 1s TTL cache,
	// but the unlucky bid that hit expiry still paid the ~70 serial GETs (the
	// p95 tail). Now the bid path NEVER touches Redis: RefreshSpend (the
	// background bulk-MGET refresher, one RTT/tick) keeps this map warm, and
	// this pod's own Record/Reconcile write their exact result back so local
	// wins gate immediately. Cross-pod staleness is bounded by the refresher
	// interval (~1s) — safe, because this counter is a deliberately
	// conservative overspend guard the billing snapshot reconciles anyway.
	mu         sync.Mutex
	spendCache map[string]spendEntry // key = day-stamped budgetKey
}

type spendEntry struct {
	micros int64
	at     time.Time
}

// NewBudgetTracker wires the tracker to an L2 cache (Redis in prod, in-memory in tests).
func NewBudgetTracker(l2 cache.L2Cache, ttlFn func() time.Duration, log *slog.Logger) *BudgetTracker {
	return &BudgetTracker{l2: l2, ttlFn: ttlFn, nowFn: time.Now, log: log, spendCache: make(map[string]spendEntry)}
}

// dateFmt is the UTC day stamp shared with pkg/billing's accumulator (dayKey).
const dateFmt = "2006-01-02"

func budgetKey(day, campaignID string) string {
	return "dsp:budget:" + day + ":" + campaignID + ":spent"
}

func (b *BudgetTracker) today() string { return b.nowFn().UTC().Format(dateFmt) }

// Spend returns today's spend for a campaign in major units (dollars).
func (b *BudgetTracker) Spend(campaignID string) float64 {
	return float64(b.SpendMicros(campaignID)) / microsPerUSD
}

// SpendMicros returns today's spend for a campaign in the native fixed-point
// micro-dollar unit — no dollar rounding, so sub-cent spend ($0.005 CPM =
// 5,000µ) is exact. Used by the /debug/budget endpoint (and tests) that observe
// the counter the pacing gate and reconcile write.
//
// Reads the in-process copy ONLY — never Redis (the twice-proven rule:
// nothing in the per-campaign bid loop may do per-call network I/O). An
// unknown key reads 0 until the refresher's next tick, exactly like the old
// cached-miss behaviour; on Redis trouble the refresher keeps the last-good
// entries, so stale still beats zero.
func (b *BudgetTracker) SpendMicros(campaignID string) int64 {
	key := budgetKey(b.today(), campaignID)
	b.mu.Lock()
	e, cached := b.spendCache[key]
	b.mu.Unlock()
	if cached {
		return e.micros
	}
	return 0
}

// setSpend writes an authoritative counter value into the in-process copy —
// called with this pod's own write results (Record's INCRBY return, Reconcile's
// Set value) so local writes gate immediately without a Redis read.
func (b *BudgetTracker) setSpend(key string, micros int64) {
	b.mu.Lock()
	b.spendCache[key] = spendEntry{micros: micros, at: b.nowFn()}
	b.mu.Unlock()
}

// RefreshSpend is the background bulk refresher's entry point: one MGET for
// every live campaign's day-stamped counter, written into the in-process copy
// wholesale. Also prunes entries no longer in the key set (yesterday's keys,
// deleted campaigns) so the map tracks the catalog, not history. On error the
// existing entries are left untouched (stale beats zero on a Redis blip).
func (b *BudgetTracker) RefreshSpend(ctx context.Context, campaignIDs []string) {
	day := b.today()
	keys := make([]string, len(campaignIDs))
	for i, id := range campaignIDs {
		keys[i] = budgetKey(day, id)
	}
	vals, err := cache.MGet(ctx, b.l2, keys)
	if err != nil {
		b.log.Warn("budget bulk refresh failed; serving last-good spend", "campaigns", len(campaignIDs), "error", err)
		return
	}
	now := b.nowFn()
	want := make(map[string]struct{}, len(keys))
	b.mu.Lock()
	for i, k := range keys {
		want[k] = struct{}{}
		var micros int64
		if vals[i] != nil {
			micros, _ = strconv.ParseInt(*vals[i], 10, 64)
		}
		b.spendCache[k] = spendEntry{micros: micros, at: now}
	}
	for k := range b.spendCache {
		if _, ok := want[k]; !ok {
			delete(b.spendCache, k)
		}
	}
	b.mu.Unlock()
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

	// INCRBY returns the post-increment counter — write it straight into the
	// in-process copy so this pod's own win gates immediately without a read.
	if v, err := b.l2.IncrBy(ctx, key, micros); err != nil {
		b.log.Warn("budget incr failed", "campaign", campaignID, "error", err)
	} else {
		b.setSpend(key, v)
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
	key := budgetKey(day, campaignID)
	if err := b.l2.Set(context.Background(), key, strconv.FormatInt(micros, 10), b.ttlFn()); err != nil {
		b.log.Warn("budget reconcile failed", "campaign", campaignID, "error", err)
		return
	}
	b.setSpend(key, micros)
}
