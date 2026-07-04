package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// startBalanceSink wires the prepay drawdown into the billing engine
// (the "money loop", docs/PLAN.md): every realized spend debits
// advertiser_balances via postgres.BalanceStore, and the sink notifies the
// serving side —
//
//   - adtech.cache.invalidate.advertiser-balances, throttled per account
//     (billing.balance_invalidate_min_interval, default 5s) so the DSP's
//     balance warm cache rebases without a NATS message per impression;
//   - adtech.balance.depleted once when a balance crosses <= 0 (re-armed
//     when a later refresh/credit shows funds — the DSP also publishes its
//     own depleted signal from the bid path; consumers dedupe by account).
//
// Postgres connects lazily and self-heals (same posture as
// pickContractLoader): a sink error never fails the billing event — the
// engine ledger row is the source of truth and the debit is idempotent, so
// a reconciliation replay can recover missed drawdowns.
func startBalanceSink(cfg *config.Config, log *slog.Logger, engine *billing.Engine, bus events.EventBus) {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("balance sink disabled: database.url not set — advertiser balances will not draw down")
		return
	}
	sink := &pgBalanceSink{
		dbURL: dbURL,
		log:   log,
		bus:   bus,
		minInterval: func() time.Duration {
			return cfg.GetDuration("billing.balance_invalidate_min_interval", 5*time.Second)
		},
		lastInvalidate: map[string]time.Time{},
		depleted:       map[string]bool{},
	}
	engine.SetBalanceSink(sink)
	log.Info("balance sink wired: realized spend draws down advertiser_balances")
}

type pgBalanceSink struct {
	dbURL string
	log   *slog.Logger
	bus   events.EventBus

	mu    sync.Mutex
	store *postgres.BalanceStore

	minInterval    func() time.Duration
	lastInvalidate map[string]time.Time // accountID -> last invalidate publish
	depleted       map[string]bool      // accountID -> depleted event already fired
}

// connect lazily builds (or rebuilds after error) the Postgres store.
func (s *pgBalanceSink) connect() (*postgres.BalanceStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return s.store, nil
	}
	st, err := postgres.New(postgres.Config{PrimaryURL: s.dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
	if err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	s.store = &postgres.BalanceStore{Store: st}
	return s.store, nil
}

func (s *pgBalanceSink) Debit(ctx context.Context, advertiserID string, amount float64, currency, traceID, eventType string) (float64, bool, error) {
	store, err := s.connect()
	if err != nil {
		return 0, false, err
	}
	newBalance, applied, err := store.DebitSpend(ctx, advertiserID, amount, currency, traceID, eventType)
	if err != nil {
		// Drop the handle so the next call re-dials (self-healing).
		s.mu.Lock()
		s.store = nil
		s.mu.Unlock()
		return 0, false, err
	}
	if applied {
		s.notify(ctx, advertiserID, newBalance)
	}
	return newBalance, applied, nil
}

// notify publishes the (throttled) cache invalidate and the one-shot
// depleted event. Best-effort — the balance row is already committed.
func (s *pgBalanceSink) notify(ctx context.Context, accountID string, newBalance float64) {
	if s.bus == nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	shouldInvalidate := now.Sub(s.lastInvalidate[accountID]) >= s.minInterval()
	if shouldInvalidate {
		s.lastInvalidate[accountID] = now
	}
	fireDepleted := false
	if newBalance <= 0 && !s.depleted[accountID] {
		s.depleted[accountID] = true
		fireDepleted = true
	} else if newBalance > 0 && s.depleted[accountID] {
		s.depleted[accountID] = false // re-arm after a topup restores funds
	}
	s.mu.Unlock()

	if shouldInvalidate {
		_ = s.bus.Publish(ctx, events.SubjectCacheInvalidateAdvertiserBalances,
			[]byte(`{"source":"billing-sink","account_id":"`+accountID+`"}`))
	}
	if fireDepleted {
		pub := events.NewPublisher(s.bus, s.log)
		if err := pub.BalanceDepleted(ctx, events.BalanceDepletedEvent{
			AccountID: accountID, Balance: newBalance, Timestamp: now,
		}); err != nil {
			s.log.Error("balance depleted publish failed", "account_id", accountID, "error", err)
		} else {
			s.log.Info("advertiser balance depleted", "account_id", accountID, "balance", newBalance)
		}
	}
}
