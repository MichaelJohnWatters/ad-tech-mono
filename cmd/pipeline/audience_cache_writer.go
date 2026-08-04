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
	"time"

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

// startAudienceCacheWriter wires the single writer if a DB + Redis are available,
// returning it so main can expose a debug drain endpoint (nil if disabled).
func startAudienceCacheWriter(cfg *config.Config, log *slog.Logger, lc *lifecycle.Lifecycle) *audienceCacheWriter {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("audience cache writer: open postgres — disabled", "error", err)
		return nil
	}
	db.SetMaxOpenConns(4)
	lc.OnShutdown("audience-cache-writer-db", func(context.Context) error { return db.Close() })

	l2 := cache.NewSelfHealingL2(func(ctx context.Context) (cache.L2Cache, error) {
		return cacheredis.New(ctx, cacheredis.Config{
			Addr: keys.Redis.URL.Get(cfg), Password: keys.Redis.Password.Get(cfg), DB: keys.Redis.DB.Get(cfg),
		})
	}, 10*time.Second, keys.Redis.URL.Get(cfg), log)

	w := &audienceCacheWriter{
		store:          audiencepg.New(db),
		l2:             l2,
		log:            log,
		ttl:            keys.Audience.CacheTTL.Get(cfg),
		pollEvery:      func() time.Duration { return keys.Audience.ChangelogPoll.Get(cfg) },
		reconcileEvery: func() time.Duration { return keys.Audience.ChangelogReconcile.Get(cfg) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	lc.OnShutdown("audience-cache-writer", func(context.Context) error { cancel(); return nil })
	go w.run(ctx)
	log.Info("audience cache writer running (append-based, single writer)")
	return w
}

// DebugRefreshHandler forces a synchronous drain + reconcile so e2e tests get a
// deterministic fresh cache without waiting for the poll tick. Same route the
// DSP/SSP preloader used to expose; the harness now targets the single writer.
func (w *audienceCacheWriter) DebugRefreshHandler(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
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
	watermark      int64
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

// reconcile rebuilds every user's Redis set from Postgres truth — the self-heal
// for dropped appends / batch prunes / TTL expiry. Rewrites keys wholesale under
// a fresh TTL; stale members are dropped because a full replace clears them.
func (w *audienceCacheWriter) reconcile(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
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
	users := 0
	for key, segs := range grouped {
		if e := w.l2.Delete(rctx, key); e != nil {
			w.log.Debug("audience cache writer: reconcile delete failed", "key", key, "error", e)
		}
		if len(segs) == 0 {
			continue
		}
		if e := w.l2.SAdd(rctx, key, segs...); e != nil {
			w.log.Debug("audience cache writer: reconcile SADD failed", "key", key, "error", e)
			continue
		}
		_ = w.l2.Expire(rctx, key, w.ttl)
		users++
	}
	w.log.Info("audience cache writer: reconcile complete", "keys", users)
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
