package main

// audience_cache_writer.go — the SINGLE writer that maintains the append-based
// audience membership cache in Redis (Redis SETs keyed audience:set:{user}:{vis}).
//
// It drains audience_membership_changelog past a persisted watermark and applies
// each change as an atomic SADD/SREM — no per-user rebuild, no per-pod fan-out
// (pipeline is single-replica). A periodic full-scan reconcile rebuilds the sets
// from Postgres truth, self-healing anything a delta missed (dropped append,
// batch prune, TTL expiry).
//
// STAGE 2 (parallel run): this writes a SEPARATE key namespace (audience:set:…)
// alongside the live JSON path (audience:user:…), so it can be verified correct
// before the DSP/SSP read is flipped to SMEMBERS in stage 3. Nothing reads these
// keys yet.

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
)

const (
	// watermarkKey persists the last-applied changelog seq in Redis so the writer
	// resumes after a restart without replaying the whole log.
	watermarkKey = "audience:changelog:watermark"
	// changeBatch caps how many changelog rows one poll applies.
	changeBatch = 1000
)

// setKey is the Redis SET key holding a user's segment ids for one visibility.
func setKey(userID, visibility string) string {
	return "audience:set:" + userID + ":" + visibility
}

// waitForRedis blocks (bounded) until Redis answers a Ping, so the single writer
// starts on the real backend rather than the silent in-memory fallback. Proceeds
// anyway after the cap (self-heal keeps retrying) rather than wedging pipeline boot.
func waitForRedis(cfg cacheredis.Config, log *slog.Logger) {
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := cacheredis.New(ctx, cfg)
		cancel()
		if err == nil {
			_ = c.Close()
			return
		}
		log.Warn("audience cache writer: waiting for redis", "attempt", i, "error", err)
		time.Sleep(2 * time.Second)
	}
	log.Error("audience cache writer: redis still unreachable after wait; proceeding (self-heal will retry)")
}

// startAudienceCacheWriter wires the single writer if a DB + Redis are available,
// returning it so main can expose a debug drain endpoint (nil if disabled). reg
// receives the writer's lag/backlog gauges (nil = no metrics).
func startAudienceCacheWriter(cfg *config.Config, log *slog.Logger, lc *lifecycle.Lifecycle, reg *prometheus.Registry) *audienceCacheWriter {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("audience cache writer: open postgres — disabled", "error", err)
		return nil
	}
	db.SetMaxOpenConns(4)
	lc.OnShutdown("audience-cache-writer-db", func(context.Context) error { return db.Close() })

	redisCfg := cacheredis.Config{
		Addr: keys.Redis.URL.Get(cfg), Password: keys.Redis.Password.Get(cfg), DB: keys.Redis.DB.Get(cfg),
	}
	// The SINGLE writer must never silently run on the in-memory fallback — nothing
	// else writes the audience sets, so a fallback = lost writes. Wait for Redis to
	// be reachable before building the (self-healing) client, so it starts ON Redis.
	waitForRedis(redisCfg, log)
	l2 := cache.NewSelfHealingL2(func(ctx context.Context) (cache.L2Cache, error) {
		return cacheredis.New(ctx, redisCfg)
	}, 10*time.Second, redisCfg.Addr, log)

	w := &audienceCacheWriter{
		store:          audiencepg.New(db),
		l2:             l2,
		log:            log,
		ttl:            keys.Audience.CacheTTL.Get(cfg),
		pollEvery:      func() time.Duration { return keys.Audience.ChangelogPoll.Get(cfg) },
		reconcileEvery: func() time.Duration { return keys.Audience.ChangelogReconcile.Get(cfg) },
		lagWarn:        func() time.Duration { return keys.Audience.ChangelogLagWarn.Get(cfg) },
	}
	w.registerMetrics(reg)

	ctx, cancel := context.WithCancel(context.Background())
	lc.OnShutdown("audience-cache-writer", func(context.Context) error { cancel(); return nil })
	go w.run(ctx)
	go w.monitorLag(ctx)
	log.Info("audience cache writer running (append-based, single writer)")
	return w
}

// DebugRefreshHandler forces a synchronous drain + reconcile so e2e tests get a
// deterministic fresh cache without waiting for the poll tick. Same route the
// DSP/SSP preloader used to expose; the harness now targets the single writer.
func (w *audienceCacheWriter) DebugRefreshHandler(rw http.ResponseWriter, r *http.Request) {
	// Budget scales with the density-dependent reconcile (measured 39s at
	// 500k memberships — a fixed 60s cap made this endpoint fail on exactly
	// the dense worlds where an operator reaches for it) plus drain headroom.
	ctx, cancel := context.WithTimeout(r.Context(), w.reconcileBudget()+60*time.Second)
	defer cancel()
	w.drain(ctx)
	w.reconcile(ctx)
	rw.WriteHeader(http.StatusOK)
}

type audienceCacheWriter struct {
	store          *audiencepg.Store
	l2             cache.L2Cache
	log            *slog.Logger
	ttl            time.Duration
	pollEvery      func() time.Duration
	reconcileEvery func() time.Duration
	lagWarn        func() time.Duration
	// mu serializes drain/reconcile so the background loop and the debug-refresh
	// HTTP handler never run concurrently (they share watermark + prevKeys and
	// both write Redis).
	mu        sync.Mutex
	watermark int64
	// prevKeys is the set of Redis set-keys the last reconcile wrote. A user who
	// lost ALL memberships (prune / TTL expiry) vanishes from the scan, so their
	// stale key is tombstoned by diffing against this. Single writer → in-memory
	// is sufficient (no cross-pod coherence needed).
	prevKeys map[string]bool

	lastDrain    atomic.Int64 // unix millis of the last successful drain (writer liveness)
	backlogGauge prometheus.Gauge
	lagGauge     prometheus.Gauge
	drainAge     prometheus.Gauge
}

// registerMetrics registers the writer's lag/backlog gauges. These are the
// "is the single writer keeping up?" signals — if backlog/lag climb, shard it.
func (w *audienceCacheWriter) registerMetrics(reg *prometheus.Registry) {
	if reg == nil {
		return
	}
	g := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "adtech", Subsystem: "audience_cache", Name: name, Help: help,
			ConstLabels: prometheus.Labels{"service": "pipeline"},
		})
	}
	w.backlogGauge = g("changelog_backlog", "Un-drained audience membership change-log rows (writer behind if growing).")
	w.lagGauge = g("changelog_lag_seconds", "Age of the oldest un-drained change-log row.")
	w.drainAge = g("drain_age_seconds", "Seconds since the writer last successfully drained (liveness).")
	reg.MustRegister(w.backlogGauge, w.lagGauge, w.drainAge)
}

// monitorLag periodically publishes the backlog/lag/liveness gauges and logs a
// WARN when the oldest un-drained change is older than lagWarn — visible even
// without Grafana.
func (w *audienceCacheWriter) monitorLag(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		count, oldestSec, err := w.store.ChangelogBacklog(mctx)
		cancel()
		if err != nil {
			w.log.Debug("audience cache writer: backlog query failed", "error", err)
			continue
		}
		if w.backlogGauge != nil {
			w.backlogGauge.Set(float64(count))
			w.lagGauge.Set(oldestSec)
			if ms := w.lastDrain.Load(); ms > 0 {
				w.drainAge.Set(time.Since(time.UnixMilli(ms)).Seconds())
			}
		}
		if warn := w.lagWarn(); warn > 0 && oldestSec > warn.Seconds() {
			w.log.Warn("audience cache writer falling behind — consider sharding the writer",
				"backlog", count, "oldest_lag_seconds", oldestSec, "warn_threshold_seconds", warn.Seconds())
		}
	}
}

func (w *audienceCacheWriter) run(ctx context.Context) {
	// Reconcile once at boot so the sets are whole before deltas layer on top,
	// and load the persisted watermark.
	w.watermark = w.loadWatermark(ctx)
	w.reconcile(ctx)

	poll := time.NewTicker(w.pollEvery())
	defer poll.Stop()
	recon := time.NewTicker(w.reconcileEvery())
	defer recon.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			w.drain(ctx)
		case <-recon.C:
			w.reconcile(ctx)
		}
	}
}

// drain applies changelog rows past the watermark as atomic SADD/SREM, advances
// + persists the watermark, and trims consumed rows.
func (w *audienceCacheWriter) drain(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Redis is the source of truth for the watermark, reloaded each drain: if Redis
	// is flushed (e.g. an e2e reset) the watermark drops to 0 and we re-apply the
	// (now also truncated/restarted) change-log from the start, rather than skipping
	// low-seq rows against a stale in-memory cursor. In production seq is monotonic
	// so this is just a cheap GET.
	w.watermark = w.loadWatermark(ctx)
	w.lastDrain.Store(time.Now().UnixMilli()) // liveness: the drain loop is alive
	for {
		changes, err := w.store.ReadMembershipChangesSince(ctx, w.watermark, changeBatch)
		if err != nil {
			w.log.Warn("audience cache writer: read changelog failed", "error", err)
			return
		}
		if len(changes) == 0 {
			return
		}
		var maxSeq int64
		for _, c := range changes {
			key := setKey(c.UserID, c.Visibility)
			switch c.Op {
			case "remove":
				if e := w.l2.SRem(ctx, key, c.SegmentID); e != nil {
					w.log.Debug("audience cache writer: SREM failed", "key", key, "error", e)
				}
			default: // add
				if e := w.l2.SAdd(ctx, key, c.SegmentID); e != nil {
					w.log.Debug("audience cache writer: SADD failed", "key", key, "error", e)
				} else {
					_ = w.l2.Expire(ctx, key, w.ttl)
				}
			}
			if c.Seq > maxSeq {
				maxSeq = c.Seq
			}
		}
		w.watermark = maxSeq
		w.saveWatermark(ctx, maxSeq)
		if _, e := w.store.TrimMembershipChanges(ctx, maxSeq); e != nil {
			w.log.Debug("audience cache writer: trim failed", "error", e)
		}
		if len(changes) < changeBatch {
			return
		}
	}
}

// reconcileBudget is the time box for one full reconcile: the configured
// reconcile interval (floor 60s). A HARD 60s here was a scale cliff, found
// in the 2026-08-09 density run: at 500k memberships the reconcile measured
// 39s, so somewhere past ~1M the old budget would kill it MID-LOOP — and a
// key the loop never reaches gets no TTL refresh, so after cache_ttl its
// members silently vanish from serving and only a COMPLETED reconcile can
// bring them back. The budget must scale with the work; interval-sized
// keeps a permanently-overrunning reconcile from stacking behind the mutex.
func (w *audienceCacheWriter) reconcileBudget() time.Duration {
	if iv := w.reconcileEvery(); iv > 60*time.Second {
		return iv
	}
	return 60 * time.Second
}

// reconcile rebuilds every user's Redis set from Postgres truth — the self-heal
// for dropped appends / batch prunes / TTL expiry. Rewrites keys wholesale under
// a fresh TTL; stale members are dropped because a full replace clears them.
func (w *audienceCacheWriter) reconcile(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, w.reconcileBudget())
	defer cancel()
	rows, err := w.store.AllMemberships(rctx)
	if err != nil {
		w.log.Warn("audience cache writer: reconcile scan failed", "error", err)
		return
	}
	grouped := make(map[string][]string, len(rows))
	for _, r := range rows {
		key := setKey(r.UserID, r.Visibility)
		grouped[key] = append(grouped[key], r.SegmentID)
	}
	current := make(map[string]bool, len(grouped))
	users := 0
	rewritten := 0
	for key, segs := range grouped {
		// Diff-and-skip: most sets don't change between reconciles, and a
		// wholesale ReplaceSet (MULTI: DEL+SADD+EXPIRE) per user per cycle is
		// pure Redis write churn — at fleet scale it showed up as slowlog
		// entries and event-loop pressure. Read the live set first; when the
		// membership already matches, just refresh the TTL (so an unchanged
		// user doesn't expire before the next reconcile) and move on.
		if live, e := w.l2.SMembers(rctx, key); e == nil && sameMembers(live, segs) {
			if e := w.l2.Expire(rctx, key, w.ttl); e == nil {
				current[key] = true
				users++
				continue
			}
			// Expire failed (e.g. key vanished between reads) → fall through
			// to the full rewrite below.
		}
		// Atomic replace (MULTI: DEL+SADD+EXPIRE) so a concurrent bid-time SMEMBERS
		// never sees an empty/half-built set during a rebuild.
		if e := w.l2.ReplaceSet(rctx, key, segs, w.ttl); e != nil {
			w.log.Debug("audience cache writer: reconcile ReplaceSet failed", "key", key, "error", e)
			continue
		}
		current[key] = true
		users++
		rewritten++
	}
	// Tombstone keys written last reconcile that have NO members now — a user
	// pruned by the profile-builder or aged out by TTL vanishes from the scan, so
	// diff against prevKeys and delete the stragglers (else they'd match forever).
	tombstoned := 0
	for key := range w.prevKeys {
		if !current[key] {
			if e := w.l2.Delete(rctx, key); e != nil {
				w.log.Debug("audience cache writer: reconcile tombstone failed", "key", key, "error", e)
				continue
			}
			tombstoned++
		}
	}
	w.prevKeys = current
	w.log.Info("audience cache writer: reconcile complete",
		"keys", users, "rewritten", rewritten, "unchanged", users-rewritten, "tombstoned", tombstoned)
}

// sameMembers reports whether the live Redis set and the Postgres-truth slice
// hold exactly the same segment ids (order-free; tolerates duplicates in the
// scan, which SADD would collapse anyway).
func sameMembers(live, truth []string) bool {
	if len(live) == 0 && len(truth) == 0 {
		return false // empty live set = missing key; let ReplaceSet decide
	}
	truthSet := make(map[string]bool, len(truth))
	for _, s := range truth {
		truthSet[s] = true
	}
	if len(live) != len(truthSet) {
		return false
	}
	for _, s := range live {
		if !truthSet[s] {
			return false
		}
	}
	return true
}

func (w *audienceCacheWriter) loadWatermark(ctx context.Context) int64 {
	v, ok, err := w.l2.Get(ctx, watermarkKey)
	if err != nil || !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (w *audienceCacheWriter) saveWatermark(ctx context.Context, seq int64) {
	if e := w.l2.Set(ctx, watermarkKey, strconv.FormatInt(seq, 10), 0); e != nil {
		w.log.Debug("audience cache writer: save watermark failed", "error", e)
	}
}
