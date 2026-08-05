package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
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

	// deltaCache is a ~1s in-process read cache over the Redis draw-down
	// counter. HasFunds runs PER ELIGIBLE CAMPAIGN PER BID inside the
	// campaign loop — with a 66-campaign catalog that was up to ~66
	// sequential Redis GETs per display bid, the same O(campaigns) pattern
	// as the 2026-08-05 BudgetTracker bug. Phase profiling caught it:
	// campaign_loop p95 500ms with the DSP only 11% CPU-busy (all Redis
	// wait). 1s staleness is safe — the gate's whole design is
	// snapshot+delta with fail-open tolerance bounded by the 30s poll;
	// RecordWin invalidates so this pod's own wins gate immediately.
	deltaMu    sync.Mutex
	deltaCache map[string]balanceDelta
}

type balanceDelta struct {
	delta int64
	at    time.Time
}

// balanceDeltaTTL bounds cross-pod staleness of the cached draw-down read.
const balanceDeltaTTL = time.Second

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
	// Cached deltas were computed against the OLD counterAt baselines —
	// serving them against the new ones double-counts wins that billing has
	// since settled into the snapshot (caught by
	// TestBalanceGate_RebaseDoesNotDoubleCount).
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

	delta := g.drawDownDelta(accountID, base.counterAt)

	// Invoiced accounts bid on credit: creditLimitMicros extends the headroom.
	// Prepay rows have creditLimitMicros == 0, so this is the old prepay gate
	// (balance − spend > 0) byte-for-byte.
	remainingMicros := base.balanceMicros + base.creditLimitMicros - delta
	return remainingMicros > 0, float64(remainingMicros) / microsPerUSD
}

// drawDownDelta returns the account's Redis draw-down beyond the snapshot
// baseline, served from the ~1s in-process cache (see deltaCache) with a
// Redis refresh on expiry. Redis errors fall open on the snapshot alone —
// unchanged posture, just centralized.
func (g *BalanceGate) drawDownDelta(accountID string, counterAt int64) int64 {
	now := time.Now()
	g.deltaMu.Lock()
	e, hit := g.deltaCache[accountID]
	g.deltaMu.Unlock()
	if hit && now.Sub(e.at) < balanceDeltaTTL {
		return e.delta
	}
	var delta int64
	if v, ok, err := g.l2.Get(context.Background(), balanceKey(accountID)); err != nil {
		g.log.Warn("balance mirror read failed; gating on snapshot only", "account", accountID, "error", err)
		if hit {
			return e.delta // stale beats zero on a Redis blip
		}
	} else if ok {
		counter, _ := strconv.ParseInt(v, 10, 64)
		if counter > counterAt {
			delta = counter - counterAt
		}
	}
	g.deltaMu.Lock()
	g.deltaCache[accountID] = balanceDelta{delta: delta, at: now}
	g.deltaMu.Unlock()
	return delta
}

// invalidateDelta drops the cached draw-down so the next read refetches —
// called after this pod's own RecordWin, keeping the local gate responsive
// despite the read cache.
func (g *BalanceGate) invalidateDelta(accountID string) {
	g.deltaMu.Lock()
	delete(g.deltaCache, accountID)
	g.deltaMu.Unlock()
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
	if _, err := g.l2.IncrBy(ctx, key, int64(math.Round(amount*microsPerUSD))); err != nil {
		g.log.Warn("balance mirror incr failed", "account", accountID, "error", err)
	}
	g.invalidateDelta(accountID)
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
