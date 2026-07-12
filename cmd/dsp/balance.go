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
}

type balanceBaseline struct {
	balanceMicros int64
	counterAt     int64
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
	return &BalanceGate{l2: l2, enabledFn: enabledFn, log: log, baselines: map[string]balanceBaseline{}}
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
			balanceMicros: int64(math.Round(b.Balance * microsPerUSD)),
			counterAt:     counter,
		}
	}
	g.mu.Lock()
	g.baselines = next
	g.mu.Unlock()
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

	var delta int64
	if v, ok, err := g.l2.Get(context.Background(), balanceKey(accountID)); err != nil {
		// Redis down: fail open on the snapshot alone (bounded by the
		// warm-cache poll interval).
		g.log.Warn("balance mirror read failed; gating on snapshot only", "account", accountID, "error", err)
	} else if ok {
		counter, _ := strconv.ParseInt(v, 10, 64)
		if counter > base.counterAt {
			delta = counter - base.counterAt
		}
	}

	remainingMicros := base.balanceMicros - delta
	return remainingMicros > 0, float64(remainingMicros) / microsPerUSD
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
}

// startBalanceGate wires the warm cache + gate. With database.url unset both
// returns are nil and the bid handler skips balance enforcement entirely —
// the same "boot regardless of infra" posture as the opt-out cache.
func startBalanceGate(cfg *config.Config, clk clock.Clock, log *slog.Logger, bus events.EventBus, l2 cache.L2Cache) (*BalanceGate, *warm.Cache[postgres.AdvertiserBalance]) {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, balance gate disabled (prepay not enforced)")
		return nil, nil
	}
	gate := NewBalanceGate(l2, func() bool { return cfg.GetBool("dsp.balance_gate_enabled", true) }, log)

	pollInterval := firstNonZeroDuration(
		cfg.GetDuration("cache.warm.advertiser_balances.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 30*time.Second),
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
