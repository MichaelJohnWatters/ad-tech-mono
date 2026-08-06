package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
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
// authoritativeSettled (nil in single-replica / shared-counter-off mode) is the
// cluster-global per-campaign settled query returned by startSharedPacingCounter.
// When present, ONLY the elected publisher persists, and it persists THAT map —
// persisting each pod's local accumulator at 3 replicas meant three partial
// views (each ~a third of the queue-group stream) overwriting one row,
// last-writer-wins: settled_micros — the number INVOICES bill from — sat at a
// uniform ~32% of cleared spend (measured 2026-08-04).
func startSpendSnapshotPublisher(engine *billing.Engine, bus events.EventBus, persistStore *lazyCommittedSpendStore, authoritativeSettled func(context.Context) (map[string]int64, error), cfg *config.Config, clk clock.Clock, log *slog.Logger, lc *lifecycle.Lifecycle) {
	if !keys.Reporting.SpendSnapshotEnabled.Get(cfg) {
		log.Info("spend snapshot publisher disabled (reporting.spend_snapshot_enabled=false)")
		return
	}
	interval := keys.Reporting.SpendSnapshotInterval.Get(cfg)
	pub := events.NewPublisher(bus, log)
	guard := newPublisherGuard(cfg, interval, log)
	if guard.rdb != nil {
		lc.OnShutdown("pacing-publisher-guard", func(_ context.Context) error { return guard.rdb.Close() })
	}

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
				if ttl := keys.Reporting.PacingHoldTTL.Get(cfg); ttl > 0 {
					engine.SetPacingHoldTTL(ttl)
				}
				won := guard.acquire()
				if won {
					publishSpendSnapshot(engine, pub, clk, log)
				}
				if authoritativeSettled != nil {
					// Shared mode: one writer, cluster-global settled. The
					// non-elected replicas persist nothing — their local maps
					// are partial views of the queue-group stream.
					if won {
						persistCommittedSpendShared(persistStore, engine, authoritativeSettled, log)
					}
				} else {
					// Single-replica mode: the local accumulator IS the whole
					// stream; every replica persisting is idempotent.
					persistCommittedSpend(persistStore, engine, log)
				}
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
	settled, reserved, err := store.Load(ctx, day)
	if err != nil {
		log.Warn("committed-spend hydrate skipped; pacing starts cold this boot", "error", err)
		return
	}
	if len(settled) == 0 && len(reserved) == 0 {
		return
	}
	engine.HydratePacing(day, settled, reserved)
	log.Info("committed-spend hydrated from postgres", "settled_campaigns", len(settled), "reserved_campaigns", len(reserved), "day", day)
}

// persistCommittedSpend writes the engine's settled-spend so the next boot can
// hydrate it. Errors log at ERROR (per feedback_failures_must_be_error_logs) but
// never disrupt the publisher — a missed persist just widens the restart-loss
// window to the next successful tick.
func persistCommittedSpend(store *lazyCommittedSpendStore, engine *billing.Engine, log *slog.Logger) {
	if store == nil {
		return
	}
	day, settled, reserved := engine.PacingState()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Save(ctx, day, settled, reserved); err != nil {
		log.Error("committed-spend persist failed", "error", err)
	}
}

// persistCommittedSpendShared persists settled from the cluster-global
// analytics query (what invoices must bill) and reserved from THIS pod's
// engine. Reserved is knowingly approximate at >1 replica (each pod holds only
// its share of open CPC/vCPM/CPA holds — same blind spot as the counter
// reconcile, which is also CPM-derived); holds are short-lived, TTL-swept, and
// zero in an all-CPM world, so the error is bounded — settled is the
// money-bearing column and is now exact.
func persistCommittedSpendShared(store *lazyCommittedSpendStore, engine *billing.Engine, settledSource func(context.Context) (map[string]int64, error), log *slog.Logger) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settled, err := settledSource(ctx)
	if err != nil {
		// Skip rather than fall back to the partial local map: a stale row is
		// recoverable next tick, a partial overwrite is the bug we fixed.
		log.Error("committed-spend persist skipped (authoritative settled query failed)", "error", err)
		return
	}
	day, _, reserved := engine.PacingState()
	if err := store.Save(ctx, day, settled, reserved); err != nil {
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
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
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

func (s *lazyCommittedSpendStore) Save(ctx context.Context, day string, settled, reserved map[string]int64) error {
	st, err := s.connect()
	if err != nil {
		return err
	}
	return s.dropOnErr(st.Save(ctx, day, settled, reserved))
}

func (s *lazyCommittedSpendStore) Load(ctx context.Context, day string) (map[string]int64, map[string]int64, error) {
	st, err := s.connect()
	if err != nil {
		return nil, nil, err
	}
	settled, reserved, err := st.Load(ctx, day)
	return settled, reserved, s.dropOnErr(err)
}

// publisherGuard makes the single-replica requirement fail LOUDLY instead of
// silently corrupting pacing. The snapshot publisher must run on exactly one
// replica (see startSpendSnapshotPublisher); if reporting is ever scaled without
// electing a single publisher, each replica holds a partial view and their
// snapshots clobber each other. Each tick a publisher claims a shared Redis
// owner key with its pod id; if it finds the key already held by a DIFFERENT
// pod, it logs an ERROR every tick so the misconfiguration is impossible to
// miss. Best-effort: no Redis → guard disabled (logged once).
type publisherGuard struct {
	rdb     *cacheredis.Client
	podID   string
	ttl     time.Duration
	log     *slog.Logger
	leading bool // last-known election state, for edge-transition logs
}

const publisherOwnerKey = "reporting:pacing:publisher_owner"

func newPublisherGuard(cfg *config.Config, interval time.Duration, log *slog.Logger) *publisherGuard {
	podID := os.Getenv("POD_NAME")
	if podID == "" {
		podID = fmt.Sprintf("pid-%d", os.Getpid())
	}
	g := &publisherGuard{podID: podID, ttl: 3 * interval, log: log}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rdb, err := cacheredis.New(ctx, cacheredis.Config{
		Addr:     keys.Redis.URL.Get(cfg),
		Password: keys.Redis.Password.Get(cfg),
		DB:       keys.Redis.DB.Get(cfg),
	})
	if err != nil {
		log.Warn("pacing publisher guard disabled: redis unavailable — a multi-replica misconfig won't be detected", "error", err)
		return g
	}
	g.rdb = rdb
	return g
}

// acquire ELECTS the snapshot publisher: SetNX on the owner key wins the
// lease; the incumbent renews; everyone else stands by (returns false) and
// re-runs for election next tick. This replaced the detect-and-scream guard
// the moment reporting scaled past one replica (2026-07-19 multi-pod run:
// both replicas published conflicting snapshots and the old guard could only
// ERROR about it). Lease TTL = 3 ticks, so a dead leader is succeeded within
// ~3 intervals; a standby missing a few snapshots is harmless (snapshots are
// wholesale recomputes). Redis down = fail-open single-publisher-UNSAFE, so
// publish anyway and log — pacing degradation beats no pacing signal at all.
func (g *publisherGuard) acquire() bool {
	if g.rdb == nil {
		return true // no redis, no election possible — publish (single-replica assumption)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ok, err := g.rdb.SetNX(ctx, publisherOwnerKey, g.podID, g.ttl)
	if err != nil {
		g.log.Warn("pacing publisher election: redis error — publishing anyway (fail-open)", "error", err)
		return true
	}
	if ok {
		if !g.leading {
			g.leading = true
			g.log.Info("pacing snapshot publisher: elected leader", "pod", g.podID)
		}
		return true
	}
	owner, found, err := g.rdb.Get(ctx, publisherOwnerKey)
	if err == nil && found && owner == g.podID {
		// Incumbent: renew the lease.
		if err := g.rdb.Set(ctx, publisherOwnerKey, g.podID, g.ttl); err != nil {
			g.log.Warn("pacing publisher election: lease renew failed", "error", err)
		}
		return true
	}
	if g.leading {
		g.leading = false
		g.log.Info("pacing snapshot publisher: standing by (another replica leads)", "pod", g.podID, "leader", owner)
	}
	return false
}

func publishSpendSnapshot(engine *billing.Engine, pub *events.Publisher, clk clock.Clock, log *slog.Logger) {
	if released := engine.SweepExpiredHolds(); released > 0 {
		log.Info("swept expired pacing reserves", "released", released)
	}
	// Reverse the escrow hold for reservations whose settle event never came
	// (memory backend only; TigerBeetle auto-voids its pending transfers).
	if released := engine.SweepExpiredReservations(); released > 0 {
		log.Info("released expired reservations", "released", released)
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
// GET returns the current per-campaign committed micro-dollars; POST forces an
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
