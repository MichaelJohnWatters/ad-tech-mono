package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// BalanceGate enforces the advertiser prepay posture on the bid path
// (the "money loop"): no funds → no bid, account-wide.
//
// Two signals compose, both off the Postgres hot path:
//
//   - a warm cache of advertiser_balances (authoritative baseline; NATS
//     invalidate on topup credit + billing drawdown, 30s poll fallback);
//   - a Redis spend mirror this DSP increments itself on every win —
//     dsp:balance:{accountID}:spent, int64 cents, the BudgetTracker shape —
//     covering the window between a win and the billing sink's drawdown
//     landing in Postgres.
//
// remaining = snapshotBalance − (counterNow − counterAtSnapshot). Every
// warm refresh re-baselines the counter, so drawdowns that have reached
// Postgres are never double-counted against the mirror.
//
// Failure posture (per docs/PLAN.md money-loop decision): Redis errors
// fail OPEN on the last snapshot (overshoot bounded by the poll interval,
// matching BudgetTracker); a MISSING balance row fails CLOSED — an account
// that never topped up must not bid.
type BalanceGate struct {
	cache     *warm.Cache[postgres.AdvertiserBalance]
	l2        cache.L2Cache
	enabledFn func() bool
	log       *slog.Logger

	mu        sync.RWMutex
	baselines map[string]balanceBaseline

	// deltaCache is the in-process copy of each account's Redis draw-down
	// beyond the snapshot baseline. HasFunds runs PER ELIGIBLE CAMPAIGN PER
	// BID inside the campaign loop — with a 66-campaign catalog that was up
	// to ~66 sequential Redis GETs per display bid, the same O(campaigns)
	// pattern as the 2026-08-05 BudgetTracker bug (campaign_loop p95 500ms,
	// DSP 11% CPU-busy = all Redis wait). The first fix was a 1s TTL cache;
	// the unlucky expiry bid still paid the serial GETs. Now the bid path
	// NEVER touches Redis: RefreshDeltas (background bulk MGET) keeps this
	// warm and RecordWin writes its own INCRBY result back. Staleness is
	// bounded by the refresher interval (~1s) — well inside the gate's
	// snapshot+delta fail-open tolerance (30s poll).
	deltaMu    sync.Mutex
	deltaCache map[string]balanceDelta

	// baselineGen guards delta writes against a concurrent rebase: a delta
	// computed against the OLD counterAt baselines must never land after
	// rebase swapped in new ones (that double-counts wins billing has since
	// settled — the TestBalanceGate_RebaseDoesNotDoubleCount trap). Writers
	// capture the generation with the baselines and drop their write if it
	// moved. rebase bumps it BEFORE clearing deltaCache so a stale write
	// either lands pre-clear (wiped) or fails the gen check.
	baselineGen atomic.Int64
}

type balanceDelta struct {
	delta int64
	at    time.Time
}

type balanceBaseline struct {
	balanceMicros int64
	// creditLimitMicros is the invoiced-account bidding headroom. Prepay rows
	// carry credit_limit 0, so this is 0 and the gate reduces to prepay: an
	// account bids while balanceMicros + creditLimitMicros − spend > 0.
	creditLimitMicros int64
	counterAt         int64
}

func balanceKey(accountID string) string {
	return "dsp:balance:" + accountID + ":spent"
}

// balanceMirrorTTL is a hygiene TTL only — the counter is re-baselined by
// every warm refresh, so an expiry just means a brief optimistic window
// until the next 30s poll. Keeps Redis from accumulating dead accounts.
const balanceMirrorTTL = 24 * time.Hour

// NewBalanceGate builds the gate around an already-started warm cache.
// Callers must register rebase as the cache's OnRefresh (startBalanceGate
// does this).
func NewBalanceGate(l2 cache.L2Cache, enabledFn func() bool, log *slog.Logger) *BalanceGate {
	return &BalanceGate{l2: l2, enabledFn: enabledFn, log: log, baselines: map[string]balanceBaseline{}, deltaCache: map[string]balanceDelta{}}
}

// rebase records, for every balance row in the fresh snapshot, the Redis
// counter value at snapshot time. Runs on the warm cache's refresh
// goroutine — off the bid hot path.
func (g *BalanceGate) rebase(ctx context.Context, rows []postgres.AdvertiserBalance) {
	next := make(map[string]balanceBaseline, len(rows))
	for _, b := range rows {
		var counter int64
		if v, ok, err := g.l2.Get(ctx, balanceKey(b.AccountID)); err == nil && ok {
			counter, _ = strconv.ParseInt(v, 10, 64)
		}
		next[b.AccountID] = balanceBaseline{
			balanceMicros:     int64(math.Round(b.Balance * microsPerUSD)),
			creditLimitMicros: int64(math.Round(b.CreditLimit * microsPerUSD)),
			counterAt:         counter,
		}
	}
	g.mu.Lock()
	g.baselines = next
	g.mu.Unlock()
	// Bump the generation FIRST (see baselineGen), then clear: cached deltas
	// were computed against the OLD counterAt baselines — serving them
	// against the new ones double-counts wins that billing has since settled
	// into the snapshot (caught by TestBalanceGate_RebaseDoesNotDoubleCount).
	// An empty delta (0) is momentarily exact here: counterAt was read at
	// snapshot time, so the refresher's next tick only adds post-snapshot wins.
	g.baselineGen.Add(1)
	g.deltaMu.Lock()
	g.deltaCache = map[string]balanceDelta{}
	g.deltaMu.Unlock()
}

// HasFunds reports whether the account may bid, and the estimated remaining
// balance in major units (for logs).
func (g *BalanceGate) HasFunds(accountID string) (bool, float64) {
	if g.enabledFn != nil && !g.enabledFn() {
		return true, 0
	}
	g.mu.RLock()
	base, known := g.baselines[accountID]
	g.mu.RUnlock()
	if !known {
		// No advertiser_balances row: the account never topped up. Prepay
		// means no funds, no bid — the deliberate fail-closed case.
		return false, 0
	}

	delta := g.drawDownDelta(accountID)

	// Invoiced accounts bid on credit: creditLimitMicros extends the headroom.
	// Prepay rows have creditLimitMicros == 0, so this is the old prepay gate
	// (balance − spend > 0) byte-for-byte.
	remainingMicros := base.balanceMicros + base.creditLimitMicros - delta
	return remainingMicros > 0, float64(remainingMicros) / microsPerUSD
}

// drawDownDelta returns the account's cached draw-down beyond the snapshot
// baseline — in-process copy ONLY, never Redis (the bid-loop rule). A miss
// (fresh boot, just-rebased, Redis blip) gates on the snapshot alone: the
// documented fail-open posture, bounded by the refresher interval.
func (g *BalanceGate) drawDownDelta(accountID string) int64 {
	g.deltaMu.Lock()
	e, hit := g.deltaCache[accountID]
	g.deltaMu.Unlock()
	if hit {
		return e.delta
	}
	return 0
}

// setDelta writes a delta computed under generation gen, dropping it if a
// rebase moved the baselines in the meantime (see baselineGen).
func (g *BalanceGate) setDelta(accountID string, delta int64, gen int64) {
	g.deltaMu.Lock()
	if g.baselineGen.Load() == gen {
		g.deltaCache[accountID] = balanceDelta{delta: delta, at: time.Now()}
	}
	g.deltaMu.Unlock()
}

// RefreshDeltas is the background bulk refresher's entry point: one MGET over
// every live account's mirror counter, deltas recomputed against the current
// baselines and written wholesale. On error the existing entries are left
// untouched (stale beats zero on a Redis blip).
func (g *BalanceGate) RefreshDeltas(ctx context.Context, accountIDs []string) {
	if len(accountIDs) == 0 {
		return
	}
	gen := g.baselineGen.Load()
	g.mu.RLock()
	counterAts := make(map[string]int64, len(accountIDs))
	for _, id := range accountIDs {
		if b, ok := g.baselines[id]; ok {
			counterAts[id] = b.counterAt
		}
	}
	g.mu.RUnlock()

	keys := make([]string, len(accountIDs))
	for i, id := range accountIDs {
		keys[i] = balanceKey(id)
	}
	vals, err := cache.MGet(ctx, g.l2, keys)
	if err != nil {
		g.log.Warn("balance mirror bulk refresh failed; gating on last-good deltas", "accounts", len(accountIDs), "error", err)
		return
	}

	g.deltaMu.Lock()
	defer g.deltaMu.Unlock()
	if g.baselineGen.Load() != gen {
		return // rebase raced this refresh; next tick recomputes against the new baselines
	}
	now := time.Now()
	for i, id := range accountIDs {
		ca, known := counterAts[id]
		if !known {
			delete(g.deltaCache, id) // no baseline row → HasFunds fails closed anyway
			continue
		}
		var delta int64
		if vals[i] != nil {
			counter, _ := strconv.ParseInt(*vals[i], 10, 64)
			if counter > ca {
				delta = counter - ca
			}
		}
		g.deltaCache[id] = balanceDelta{delta: delta, at: now}
	}
}

// RecordWin mirrors a won auction's clearing price into the Redis counter so
// sibling pods gate on it before the billing drawdown lands in Postgres.
func (g *BalanceGate) RecordWin(accountID string, amount float64) {
	if accountID == "" || amount <= 0 {
		return
	}
	ctx := context.Background()
	key := balanceKey(accountID)
	if _, ok, _ := g.l2.Get(ctx, key); !ok {
		if err := g.l2.Set(ctx, key, "0", balanceMirrorTTL); err != nil {
			g.log.Warn("balance mirror init failed", "account", accountID, "error", err)
			return
		}
	}
	// INCRBY returns the post-increment counter — recompute this account's
	// delta from it directly so the pod's own win gates immediately without
	// a Redis read (the bid path never refetches anymore).
	gen := g.baselineGen.Load()
	counter, err := g.l2.IncrBy(ctx, key, int64(math.Round(amount*microsPerUSD)))
	if err != nil {
		g.log.Warn("balance mirror incr failed", "account", accountID, "error", err)
		return
	}
	g.mu.RLock()
	base, known := g.baselines[accountID]
	g.mu.RUnlock()
	if !known {
		return // no baseline row → fail-closed regardless of the counter
	}
	var delta int64
	if counter > base.counterAt {
		delta = counter - base.counterAt
	}
	g.setDelta(accountID, delta, gen)
}

// startBalanceGate wires the warm cache + gate. With database.url unset both
// returns are nil and the bid handler skips balance enforcement entirely —
// the same "boot regardless of infra" posture as the opt-out cache.
func startBalanceGate(cfg *config.Config, clk clock.Clock, log *slog.Logger, bus events.EventBus, l2 cache.L2Cache) (*BalanceGate, *warm.Cache[postgres.AdvertiserBalance]) {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		log.Warn("database.url not set, balance gate disabled (prepay not enforced)")
		return nil, nil
	}
	gate := NewBalanceGate(l2, func() bool { return keys.DSP.BalanceGateEnabled.Get(cfg) }, log)

	pollInterval := firstNonZeroDuration(
		cfg.GetDuration(keys.DSP.WarmAdvertiserBalancesPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
	)
	loader := &warm.RetryingLoader[postgres.AdvertiserBalance]{
		Log:   log,
		KeyFn: func(b postgres.AdvertiserBalance) string { return b.AccountID },
		Construct: func() (warm.Loader[postgres.AdvertiserBalance], error) {
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.BalanceLoader{Store: store}, nil
		},
	}
	c := warm.New(warm.Config[postgres.AdvertiserBalance]{
		Name:              "advertiser_balances",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateAdvertiserBalances,
		PollInterval:      pollInterval,
		Log:               log,
		OnRefresh:         gate.rebase,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("balance cache initial load failed", "error", err)
	}
	gate.cache = c
	return gate, c
}
