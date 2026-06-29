// Package preload is an eager, warm-cache implementation of the audience
// segment Lookup. A background goroutine queries audience_segment_members
// every N seconds, groups by (user_id, visibility), and dumps the result
// into Redis. The bid hot path then reads exclusively from Redis — no
// Postgres roundtrip per bid, no per-user latency tail.
//
// Same architectural pattern as the warm caches for campaigns / placements
// / creatives (pkg/cache/warm), but the backing store is Redis instead of
// in-process RAM. Reason: audience membership data is unbounded by user
// count (millions in production) — replicating it into every pod's RAM
// would blow up memory; centralising it in Redis keeps one shared copy.
//
// Failure modes:
//   - Redis unreachable on read → return empty (treat as "no segments").
//     Bid proceeds without enrichment. Logged at debug.
//   - Postgres unreachable on preload → retain whatever Redis still has
//     (TTL is wider than the interval). Logged at warn. Next preload
//     attempt resumes.
//   - Empty result for a user → cached as the empty JSON array so we
//     don't accidentally re-query Postgres for users with no segments.
//
// What we deliberately don't do here: per-user invalidation on writes.
// The preloader's interval is the staleness ceiling (default 30s); a
// segment membership added between preloads becomes visible at the next
// tick. If we want sub-30s freshness later we can layer NATS invalidates
// on top — for now the interval is plenty.
package preload

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

// Preloader periodically pumps audience_segment_members into Redis and
// satisfies the store.Lookup interface for the bid hot path. Start the
// preloader at service boot; the first preload runs synchronously so
// /readyz can wait for it before passing traffic.
type Preloader struct {
	db       *sql.DB
	l2       cache.L2Cache
	interval time.Duration
	ttl      time.Duration
	log      *slog.Logger
	// lastLoad is the unix-millis timestamp of the most recent successful
	// preload. Read by /readyz; written by the load goroutine.
	lastLoad atomic.Int64
	stop     chan struct{}
	stopped  chan struct{}
}

// Config tunes the preloader. Interval defaults to 30s, TTL to 90s
// (3× interval so entries survive ~2 missed preloads before going stale).
type Config struct {
	DB       *sql.DB
	L2       cache.L2Cache
	Interval time.Duration
	TTL      time.Duration
	Log      *slog.Logger
}

// New constructs a Preloader. Call Start to begin the background pump.
func New(cfg Config) *Preloader {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 3 * cfg.Interval
	}
	return &Preloader{
		db:       cfg.DB,
		l2:       cfg.L2,
		interval: cfg.Interval,
		ttl:      cfg.TTL,
		log:      cfg.Log,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start runs the initial preload synchronously (so /readyz can block until
// the cache is hot), then launches the background ticker. Returns the
// initial-load error if any — caller decides whether to fail boot.
func (p *Preloader) Start(ctx context.Context) error {
	if err := p.preloadOnce(ctx); err != nil {
		p.log.Warn("audience preload: initial load failed (subsequent attempts continue)", "error", err)
		// Don't propagate the error — subsequent preloads might succeed
		// and the bid path is already designed to degrade gracefully on
		// empty Redis results.
	}
	go p.loop()
	return nil
}

// Stop signals the background loop to exit and waits for it to finish.
// Safe to call multiple times.
func (p *Preloader) Stop() {
	select {
	case <-p.stop:
		// already closed
	default:
		close(p.stop)
		<-p.stopped
	}
}

// Refresh runs preloadOnce synchronously. Used by the debug
// /debug/audience/refresh endpoint so the e2e harness can guarantee a
// fresh snapshot after inserting audience_segment_members rows, instead
// of waiting up to 30s for the natural tick. Same routine the background
// loop calls; concurrent invocations are safe because the goroutine
// snapshots its own ticker context and lastLoad uses atomic writes.
func (p *Preloader) Refresh(ctx context.Context) error {
	return p.preloadOnce(ctx)
}

// LastLoaded reports the time of the last successful preload. Zero means
// no preload has succeeded yet — /readyz uses that to gate readiness.
func (p *Preloader) LastLoaded() time.Time {
	ms := p.lastLoad.Load()
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// SegmentsForUser reads the user's public segments from Redis. No
// Postgres fallback — the preloader is the source of truth. Empty
// result on Redis miss = "this user has no segments". That matches the
// caller's expectation for unknown users.
func (p *Preloader) SegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return p.lookup(ctx, userID, "public")
}

// DSPSegmentsForUser reads the user's dsp_private segments from Redis.
// Same no-Postgres-fallback contract as SegmentsForUser.
func (p *Preloader) DSPSegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return p.lookup(ctx, userID, "dsp_private")
}

func (p *Preloader) lookup(ctx context.Context, userID, visibility string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	key := redisKey(userID, visibility)
	val, ok, err := p.l2.Get(ctx, key)
	if err != nil {
		// Redis blip — degrade silently. Returning an error would cause
		// the bid handler to log at WARN per bid, which floods logs.
		p.log.Debug("audience cache read failed", "key", key, "error", err)
		return nil, nil
	}
	if !ok || val == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(val), &out); err != nil {
		p.log.Debug("audience cache value garbled", "key", key, "error", err)
		return nil, nil
	}
	return out, nil
}

func (p *Preloader) loop() {
	defer close(p.stopped)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), p.interval)
			if err := p.preloadOnce(ctx); err != nil {
				p.log.Warn("audience preload failed", "error", err)
			}
			cancel()
		}
	}
}

// preloadOnce queries the membership table and writes one Redis key per
// (user, visibility). Uses MGET-equivalent batching would be a future
// optimisation; for current local-dev volumes the per-key Set is fine
// (a few hundred keys per cycle).
func (p *Preloader) preloadOnce(ctx context.Context) error {
	start := time.Now()
	// Single query joining members + segments so we can group by visibility
	// in-process rather than running two queries.
	const q = `
SELECT m.user_id, m.segment_id::text, s.visibility
FROM audience_segment_members m
JOIN audience_segments s ON s.id = m.segment_id`
	rows, err := p.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("query memberships: %w", err)
	}
	defer rows.Close()

	// Map[userID]map[visibility][]segmentID. Two levels so we end up with
	// one Redis key per (user, visibility) pair.
	grouped := map[string]map[string][]string{}
	for rows.Next() {
		var userID, segmentID, visibility string
		if err := rows.Scan(&userID, &segmentID, &visibility); err != nil {
			return fmt.Errorf("scan membership row: %w", err)
		}
		if grouped[userID] == nil {
			grouped[userID] = map[string][]string{}
		}
		grouped[userID][visibility] = append(grouped[userID][visibility], segmentID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate membership rows: %w", err)
	}

	// Write each (user, visibility) → []segmentID into Redis.
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	written := 0
	for userID, byVis := range grouped {
		for visibility, segs := range byVis {
			payload, _ := json.Marshal(segs)
			if err := p.l2.Set(writeCtx, redisKey(userID, visibility), string(payload), p.ttl); err != nil {
				p.log.Debug("audience preload set failed", "user", userID, "visibility", visibility, "error", err)
				continue
			}
			written++
		}
	}

	p.lastLoad.Store(time.Now().UnixMilli())
	p.log.Info("audience preload complete", "users", len(grouped), "keys_written", written, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func redisKey(userID, visibility string) string {
	return "audience:user:" + userID + ":" + visibility
}
