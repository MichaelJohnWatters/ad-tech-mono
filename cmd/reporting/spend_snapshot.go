package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// startSpendSnapshotPublisher periodically broadcasts the billing engine's
// per-campaign committed spend (settled-today + open reserves) so every DSP
// pod can reconcile its pacing budget counter to billed reality instead of the
// raw win-notice decrement it maintains locally. Each tick also sweeps expired
// reserves — impressions whose billable click/conversion/view never arrived —
// so a never-settled hold doesn't pace a campaign forever.
//
// Mirrors startRollupScheduler: config-gated, single ticker, clean shutdown via
// a stop channel + lifecycle hook.
//
// SINGLE-REPLICA by design (like cmd/identity-consumer): the billing engine's
// committed accumulator only sees the events THIS pod consumed, and reporting
// consumes on a shared queue group, so N replicas would each hold a partial view
// and publish conflicting partial snapshots. The base manifest pins replicas: 1
// (also required by the DuckDB single-writer constraint). If reporting is ever
// scaled, the snapshot publisher must move to a single elected replica or derive
// committed from the shared analytics store.
//
// The persistStore (may be nil) backs restart-safety: each tick persists the
// settled portion, and boot calls hydrateCommittedSpend before consumption so a
// restart doesn't reset committed to zero (which would reconcile DSPs down).
func startSpendSnapshotPublisher(engine *billing.Engine, bus events.EventBus, persistStore *lazyCommittedSpendStore, cfg *config.Config, clk clock.Clock, log *slog.Logger, lc *lifecycle.Lifecycle) {
	if !cfg.GetBool("reporting.spend_snapshot_enabled", true) {
		log.Info("spend snapshot publisher disabled (reporting.spend_snapshot_enabled=false)")
		return
	}
	interval := cfg.GetDuration("reporting.spend_snapshot_interval", 30*time.Second)
	pub := events.NewPublisher(bus, log)

	stop := make(chan struct{})
	lc.OnShutdown("spend-snapshot-publisher", func(_ context.Context) error {
		close(stop)
		return nil
	})
	go func() {
		ticker := clk.NewTicker(interval)
		defer ticker.Stop()
		log.Info("spend snapshot publisher started", "interval", interval.String())
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// Re-apply the hold TTL each tick so the TierLive config is
				// genuinely live (was boot-only). Cheap; sweeps use it below.
				if ttl := cfg.GetDuration("reporting.pacing_hold_ttl", 15*time.Minute); ttl > 0 {
					engine.SetPacingHoldTTL(ttl)
				}
				publishSpendSnapshot(engine, pub, clk, log)
				persistCommittedSpend(persistStore, engine, log)
			}
		}
	}()
}

// hydrateCommittedSpend loads today's persisted settled spend into the engine so
// a restart resumes the day's committed total instead of resetting to zero. MUST
// run before event consumption starts (it sets rather than merges). Best-effort:
// a Postgres miss just means pacing starts cold this boot (the pre-fix behaviour,
// no regression).
func hydrateCommittedSpend(store *lazyCommittedSpendStore, engine *billing.Engine, clk clock.Clock, log *slog.Logger) {
	if store == nil {
		return
	}
	day := clk.Now().UTC().Format("2006-01-02")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := store.Load(ctx, day)
	if err != nil {
		log.Warn("committed-spend hydrate skipped; pacing starts cold this boot", "error", err)
		return
	}
	if len(m) == 0 {
		return
	}
	engine.HydrateSettled(day, m)
	log.Info("committed-spend hydrated from postgres", "campaigns", len(m), "day", day)
}

// persistCommittedSpend writes the engine's settled-spend so the next boot can
// hydrate it. Errors log at ERROR (per feedback_failures_must_be_error_logs) but
// never disrupt the publisher — a missed persist just widens the restart-loss
// window to the next successful tick.
func persistCommittedSpend(store *lazyCommittedSpendStore, engine *billing.Engine, log *slog.Logger) {
	if store == nil {
		return
	}
	day, cents := engine.SettledToday()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Save(ctx, day, cents); err != nil {
		log.Error("committed-spend persist failed", "error", err)
	}
}

// lazyCommittedSpendStore dials Postgres on first use and re-dials after an
// error (mirrors lazyReservationStore / pgBalanceSink), so reporting boots
// before Postgres is reachable.
type lazyCommittedSpendStore struct {
	dbURL string
	log   *slog.Logger
	mu    sync.Mutex
	store *postgres.CommittedSpendStore
}

// newCommittedSpendStore returns nil when database.url is unset — persistence is
// simply off (pacing still works, just not restart-safe), same posture as the
// balance sink / reservation store.
func newCommittedSpendStore(cfg *config.Config, log *slog.Logger) *lazyCommittedSpendStore {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("committed-spend persistence disabled: database.url not set — DSP pacing resets on a reporting restart")
		return nil
	}
	return &lazyCommittedSpendStore{dbURL: dbURL, log: log}
}

func (s *lazyCommittedSpendStore) connect() (*postgres.CommittedSpendStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return s.store, nil
	}
	st, err := postgres.New(postgres.Config{PrimaryURL: s.dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
	if err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	s.store = &postgres.CommittedSpendStore{Store: st}
	return s.store, nil
}

func (s *lazyCommittedSpendStore) dropOnErr(err error) error {
	if err != nil {
		s.mu.Lock()
		s.store = nil
		s.mu.Unlock()
	}
	return err
}

func (s *lazyCommittedSpendStore) Save(ctx context.Context, day string, cents map[string]int64) error {
	st, err := s.connect()
	if err != nil {
		return err
	}
	return s.dropOnErr(st.Save(ctx, day, cents))
}

func (s *lazyCommittedSpendStore) Load(ctx context.Context, day string) (map[string]int64, error) {
	st, err := s.connect()
	if err != nil {
		return nil, err
	}
	m, err := st.Load(ctx, day)
	return m, s.dropOnErr(err)
}

func publishSpendSnapshot(engine *billing.Engine, pub *events.Publisher, clk clock.Clock, log *slog.Logger) {
	if released := engine.SweepExpiredHolds(); released > 0 {
		log.Info("swept expired pacing reserves", "released", released)
	}
	committed := engine.SnapshotCommitted()
	if len(committed) == 0 {
		return // nothing billed today yet — no DSP counter to correct
	}
	now := clk.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pub.CampaignSpendSnapshot(ctx, events.CampaignSpendSnapshotEvent{
		Day:       now.Format("2006-01-02"),
		Currency:  "USD",
		Committed: committed,
		Timestamp: now,
	}); err != nil {
		log.Error("publish spend snapshot failed", "campaigns", len(committed), "error", err)
		return
	}
	log.Debug("published spend snapshot", "campaigns", len(committed))
}

// spendSnapshotDebugHandler exposes the billing engine's committed-spend view.
// GET returns the current per-campaign committed cents; POST forces an
// immediate publish (used by e2e to drive DSP reconciliation without waiting
// for the ticker) and returns the same map. bus may be nil (NATS down) — GET
// still works; POST reports that it couldn't publish.
func spendSnapshotDebugHandler(engine *billing.Engine, bus events.EventBus, clk clock.Clock, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		published := false
		if r.Method == http.MethodPost {
			if bus != nil {
				publishSpendSnapshot(engine, events.NewPublisher(bus, log), clk, log)
				published = true
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"committed": engine.SnapshotCommitted(),
			"published": published,
		})
	}
}
