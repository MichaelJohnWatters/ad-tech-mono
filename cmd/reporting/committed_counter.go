package main

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// committedRedis is the subset of the Redis client the shared committed counter
// needs. The concrete *cacheredis.Client satisfies it; a fake satisfies it in
// tests.
type committedRedis interface {
	IncrBy(ctx context.Context, key string, n int64) (int64, error)
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	SAdd(ctx context.Context, key string, members ...string) error
	SMembers(ctx context.Context, key string) ([]string, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
}

// redisCommittedCounter is the Redis-backed billing.CommittedCounter that lets
// reporting run more than one replica without fragmenting pacing (see
// pkg/billing/committed.go for the why). Each campaign's committed micro-dollars
// live in their own INT64 key (billing:committed:{day}:{cid}) mutated with atomic
// INCRBY — commutative, so N replicas' partial deltas sum to the correct total —
// and the day's live campaign ids are tracked in a companion SET
// (billing:committed:{day}:index) so Snapshot can enumerate them. Every key
// carries a TTL a little over a day so yesterday's counters self-expire, mirroring
// the DSP budget key's daily rollover.
type redisCommittedCounter struct {
	rdb committedRedis
	ttl time.Duration
}

func newRedisCommittedCounter(rdb committedRedis, ttl time.Duration) *redisCommittedCounter {
	if ttl <= 0 {
		ttl = 26 * time.Hour
	}
	return &redisCommittedCounter{rdb: rdb, ttl: ttl}
}

func committedKey(day, cid string) string { return "billing:committed:" + day + ":" + cid }
func committedIndexKey(day string) string { return "billing:committed:" + day + ":index" }

// AddDelta applies signed per-campaign deltas via atomic INCRBY. Errors are
// collected but every campaign is attempted, so one bad key doesn't strand the
// rest; the caller (the engine) logs and the periodic reconcile heals drift.
func (r *redisCommittedCounter) AddDelta(ctx context.Context, day string, deltas map[string]int64) error {
	idx := committedIndexKey(day)
	var firstErr error
	for cid, delta := range deltas {
		if cid == "" || delta == 0 {
			continue
		}
		if err := r.rdb.SAdd(ctx, idx, cid); err != nil && firstErr == nil {
			firstErr = err
		}
		key := committedKey(day, cid)
		if _, err := r.rdb.IncrBy(ctx, key, delta); err != nil && firstErr == nil {
			firstErr = err
		}
		_ = r.rdb.Expire(ctx, key, r.ttl)
	}
	_ = r.rdb.Expire(ctx, idx, r.ttl)
	return firstErr
}

// Snapshot enumerates the day's campaigns from the index set and reads each
// counter, returning the combined committed total across every replica.
func (r *redisCommittedCounter) Snapshot(ctx context.Context, day string) (map[string]int64, error) {
	cids, err := r.rdb.SMembers(ctx, committedIndexKey(day))
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(cids))
	for _, cid := range cids {
		v, ok, err := r.rdb.Get(ctx, committedKey(day, cid))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		micros, _ := strconv.ParseInt(v, 10, 64)
		out[cid] = micros
	}
	return out, nil
}

// Reconcile overwrites each campaign's counter to the authoritative value with a
// plain SET, resetting any additive drift.
func (r *redisCommittedCounter) Reconcile(ctx context.Context, day string, totals map[string]int64) error {
	idx := committedIndexKey(day)
	var firstErr error
	for cid, total := range totals {
		if cid == "" {
			continue
		}
		if err := r.rdb.SAdd(ctx, idx, cid); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := r.rdb.Set(ctx, committedKey(day, cid), strconv.FormatInt(total, 10), r.ttl); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	_ = r.rdb.Expire(ctx, idx, r.ttl)
	return firstErr
}

// startSharedPacingCounter wires the shared, cross-replica committed-spend
// counter into the billing engine. OPT-IN via reporting.shared_pacing_counter
// (default off) — enable it only when running >1 reporting replica. When on it:
//  1. points the engine's SnapshotCommitted at Redis, so every replica publishes
//     the same combined total instead of its own partial in-memory view;
//  2. seeds the counter from the analytics store on boot (so a cold start /
//     restart doesn't reconcile DSP budgets down to zero);
//  3. runs a periodic Reconcile that recomputes the authoritative per-campaign
//     total from the store and resets the counter, sweeping additive drift.
//
// When off, or if Redis is unreachable, the engine keeps its in-memory
// accumulator and single-replica behaviour is completely unchanged.
func startSharedPacingCounter(engine *billing.Engine, store analytics.Store, cfg *config.Config, clk clock.Clock, log *slog.Logger, lc *lifecycle.Lifecycle) {
	if !keys.Reporting.SharedPacingCounter.Get(cfg) {
		return
	}
	addr := keys.Redis.URL.Get(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	rdb, err := cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: keys.Redis.Password.Get(cfg), DB: keys.Redis.DB.Get(cfg)})
	cancel()
	if err != nil {
		// Fail-open: without Redis the engine falls back to the in-memory
		// accumulator. That is only correct at 1 replica, so log LOUD — running
		// multiple replicas past this point risks pacing fragmentation.
		log.Error("shared pacing counter enabled but Redis unreachable; falling back to in-memory accumulator (SAFE ONLY AT 1 REPLICA)", "addr", addr, "error", err)
		return
	}
	ttl := keys.Reporting.PacingCounterTTL.Get(cfg)
	counter := newRedisCommittedCounter(rdb, ttl)
	engine.SetCommittedCounter(counter)
	log.Info("shared pacing counter enabled (Redis-backed, multi-replica safe)", "addr", addr)

	reader, hasReader := store.(analytics.CommittedReader)
	if !hasReader {
		log.Warn("analytics backend has no CommittedReader; shared pacing counter runs additive-only (no store self-heal)")
	}
	reconcile := func() {
		if !hasReader {
			return
		}
		day := clk.Now().UTC().Format("2006-01-02")
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rcancel()
		totals, err := reader.CommittedByCampaign(rctx, day)
		if err != nil {
			log.Error("pacing counter reconcile: store query failed", "day", day, "error", err)
			return
		}
		if err := counter.Reconcile(rctx, day, totals); err != nil {
			log.Error("pacing counter reconcile: counter reset failed", "day", day, "error", err)
			return
		}
		log.Debug("pacing counter reconciled from store", "campaigns", len(totals), "day", day)
	}
	reconcile() // boot seed, before the publisher starts broadcasting

	interval := keys.Reporting.PacingReconcileInterval.Get(cfg)
	stop := make(chan struct{})
	lc.OnShutdown("pacing-counter-reconcile", func(_ context.Context) error {
		close(stop)
		return rdb.Close()
	})
	go func() {
		ticker := clk.NewTicker(interval)
		defer ticker.Stop()
		log.Info("pacing counter reconcile loop started", "interval", interval.String())
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				reconcile()
			}
		}
	}()
}
