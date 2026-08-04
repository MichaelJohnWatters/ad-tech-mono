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
// Freshness model: the periodic scan is a RECONCILER, not the primary freshness
// path. Membership writers publish adtech.cache.invalidate.audience naming WHICH
// users/segment changed (events.AudienceInvalidateEvent); the preloader re-
// materializes only those Redis keys — O(changed users) — within ~a second. The
// full scan then runs on a much longer interval purely as a self-heal for anything
// a delta misses (silent TTL expiry, batch prunes, a dropped event). An id-less
// invalidate still triggers a full refresh, so older publishers keep working.
//
// Scaling notes + future work (write-through for retargeting, visibility-aware
// invalidates, bounded L1 LRU, and the Aerospike / KV-as-truth / user-sharding
// frontiers): docs/AUDIENCE_DATA_PATH_SCALING.md.
package preload

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/podid"
)

// allVisibilities is every visibility a (user → segments) Redis key can carry.
// A targeted delta refresh writes/tombstones both so a user removed from their
// last segment of a given visibility is negative-cached, not left stale.
var allVisibilities = []string{"public", "dsp_private"}

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

	// mu serializes preload cycles (the interval loop and the debug
	// /refresh handler can overlap); prevKeys is the key set written by the
	// previous cycle, diffed to tombstone users whose memberships vanished.
	mu       sync.Mutex
	prevKeys map[string]bool

	// refreshQueued coalesces invalidate bursts (the profile-builder
	// publishes one message per changed segment) into a single debounced
	// refresh.
	refreshQueued atomic.Bool
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
	// The periodic scan is now a RECONCILER, not the freshness mechanism (deltas
	// keep changed users fresh in seconds). A stable, unchanged user is only re-SET
	// once per reconcile, so the TTL must comfortably outlive the reconcile interval
	// or stable members would expire and stop matching between reconciles.
	if cfg.TTL < 2*cfg.Interval {
		cfg.TTL = 2 * cfg.Interval
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
	// NOTE: still reads the JSON path (audience:user). The append-based Redis-SET
	// path (audience:set, maintained by cmd/pipeline's single writer via the
	// change-log trigger) runs in PARALLEL and is verified matching — but the read
	// is NOT flipped to SMEMBERS yet: the writer's reconcile does a non-atomic
	// Delete+SAdd (transient-empty window) and the writer's Redis connection needs
	// reliability hardening before it can back the bid hot path. See
	// docs/AUDIENCE_DATA_PATH_SCALING.md. Flip setRedisKey/SMembers here once hardened.
	key := redisKey(userID, visibility)
	val, ok, err := p.l2.Get(ctx, key)
	if err != nil {
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

// SubscribeInvalidate wires adtech.cache.invalidate.audience to a debounced
// refresh, closing the gap between a membership write (upload, drop-zone,
// profile-builder) and the bid path seeing it: seconds instead of the poll
// interval. Per-POD consumer for broadcast semantics — same rationale as
// pkg/cache/warm: a shared group would load-balance invalidates so only one
// pod refreshed per message. EPHEMERAL via SubscribeBroadcast (the c751a52
// leak rule: a durable consumer keyed by the ever-changing pod name is never
// reaped and leaks one JetStream consumer per pod incarnation). Failure
// degrades to poll-only (the 30s interval remains the staleness ceiling
// either way).
func (p *Preloader) SubscribeInvalidate(ctx context.Context, bus events.EventBus, service string) {
	if bus == nil {
		return
	}
	// Per-REPLICA name (hostname), NOT POD_NAME: POD_NAME is shared across a
	// service's replicas, which would queue-group them so only one refreshed.
	name := service + "-audience-" + podid.Replica()
	err := events.SubscribeBroadcast(ctx, bus, events.SubjectCacheInvalidateAudience, name, func(_ context.Context, msg *events.Message) error {
		// Delta refresh: re-materialize only what changed (the writer named the
		// users or the segment). Only an id-less/garbled payload falls back to the
		// debounced full reconcile — the expensive path we're avoiding on the
		// hot membership-change loop.
		plan := planFromInvalidate(msg.Data)
		switch {
		case len(plan.userIDs) > 0:
			dctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			if err := p.refreshUsers(dctx, plan.userIDs); err != nil {
				p.log.Debug("audience delta refresh (users) failed — reconcile will catch it", "error", err)
			}
			cancel()
		case plan.segment != "":
			dctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			if err := p.refreshSegment(dctx, plan.segment); err != nil {
				p.log.Debug("audience delta refresh (segment) failed — reconcile will catch it", "error", err)
			}
			cancel()
		default:
			p.requestRefresh()
		}
		_ = msg.Ack()
		return nil
	})
	if err != nil {
		p.log.Warn("audience invalidate subscribe failed (poll-only)", "error", err)
		return
	}
	p.log.Info("audience preloader subscribed to invalidates (ephemeral)", "name", name)
}

// requestRefresh schedules one debounced preload (~1s) — a burst of
// invalidates becomes a single refresh; preloadOnce's mutex serializes it
// against the interval loop.
func (p *Preloader) requestRefresh() {
	if !p.refreshQueued.CompareAndSwap(false, true) {
		return
	}
	go func() {
		time.Sleep(time.Second)
		p.refreshQueued.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.preloadOnce(ctx); err != nil {
			p.log.Warn("audience preload (invalidate-triggered) failed", "error", err)
		}
	}()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	start := time.Now()
	// Single query joining members + segments so we can group by visibility
	// in-process rather than running two queries.
	const q = `
SELECT m.user_id, m.segment_id::text, s.visibility
FROM audience_segment_members m
JOIN audience_segments s ON s.id = m.segment_id
WHERE m.expires_at IS NULL OR m.expires_at > now()`
	// Cross-tenant preload (every account's memberships → the shared stamping
	// cache) → platform hatch, held open while scanning (security #77).
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin membership preload: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return fmt.Errorf("membership preload platform-read: %w", err)
	}
	rows, err := tx.QueryContext(ctx, q)
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
	current := make(map[string]bool, len(grouped))
	for userID, byVis := range grouped {
		for visibility, segs := range byVis {
			key := redisKey(userID, visibility)
			current[key] = true
			payload, _ := json.Marshal(segs)
			if err := p.l2.Set(writeCtx, key, string(payload), p.ttl); err != nil {
				p.log.Debug("audience preload set failed", "user", userID, "visibility", visibility, "error", err)
				continue
			}
			written++
		}
	}

	// Tombstone keys that existed last cycle but have no memberships now —
	// a pruned user (replace-by-segment, GDPR purge) must stop matching at
	// the NEXT preload, not when the TTL runs out. Overwrite with the empty
	// array (the standard negative-cache value) rather than deleting, so the
	// read path still short-circuits without Postgres.
	//
	// The "last cycle" key set lives in SHARED Redis, not per-pod memory:
	// with multiple SSP replicas behind one Redis, the pod that handles a
	// /refresh may not be the pod that wrote a key, so a per-pod prevKeys would
	// never tombstone it and a pruned user would keep matching forever. Every
	// pod reads the same Postgres, so all compute an identical `current` — the
	// shared set converges regardless of which pod runs. Union in the local set
	// too, so a one-off shared-key read failure still clears this pod's own
	// prior writes.
	prev := p.loadPrevKeys(writeCtx)
	for key := range p.prevKeys {
		prev[key] = true
	}
	tombstoned := 0
	for _, key := range keysToTombstone(prev, current) {
		if err := p.l2.Set(writeCtx, key, "[]", p.ttl); err != nil {
			p.log.Debug("audience preload tombstone failed", "key", key, "error", err)
			continue
		}
		tombstoned++
	}
	p.prevKeys = current
	p.savePrevKeys(writeCtx, current)

	p.lastLoad.Store(time.Now().UnixMilli())
	p.log.Info("audience preload complete", "users", len(grouped), "keys_written", written, "keys_tombstoned", tombstoned, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// refreshPlan is the decision derived from an invalidate payload (pure, so it's
// unit-testable): re-materialize a specific set of users, a whole segment's users,
// or fall back to a full reconcile.
type refreshPlan struct {
	full    bool
	userIDs []string
	segment string
}

// planFromInvalidate decodes an AudienceInvalidateEvent into a refreshPlan.
// UserIDs win (precise, handles removals); else a SegmentID; else full reconcile
// (empty/garbled payload, or a legacy publisher that named no ids).
func planFromInvalidate(data []byte) refreshPlan {
	var ev events.AudienceInvalidateEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return refreshPlan{full: true}
	}
	if len(ev.UserIDs) > 0 {
		return refreshPlan{userIDs: ev.UserIDs}
	}
	if ev.SegmentID != "" {
		return refreshPlan{segment: ev.SegmentID}
	}
	return refreshPlan{full: true}
}

// refreshUsers re-materializes the Redis keys for exactly these users — O(changed
// users), not O(all members). Each user's FULL current segment list (across all
// segments) is re-read and written; a visibility with no memberships is negative-
// cached ("[]") so a suppressed/removed user stops matching immediately.
func (p *Preloader) refreshUsers(ctx context.Context, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	grouped, err := p.readUserMemberships(ctx, userIDs)
	if err != nil {
		return err
	}
	written, tombstoned := p.writeUsers(ctx, userIDs, grouped)
	p.log.Debug("audience delta refresh (users)", "users", len(userIDs), "keys_written", written, "keys_tombstoned", tombstoned)
	return nil
}

// refreshSegment re-materializes every user currently in a segment (bounded by
// segment size). Used for bulk changes (uploads, profile-builder rebuilds) where
// listing individual user ids in the event would be too large. Only sees CURRENT
// members, so it covers additions; removals ride the user-targeted path instead.
func (p *Preloader) refreshSegment(ctx context.Context, segmentID string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	users, err := p.readSegmentUsers(ctx, segmentID)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	return p.refreshUsers(ctx, users)
}

// readUserMemberships returns userID → visibility → []segmentID for the given
// users, in one cross-tenant (platform-read) query.
func (p *Preloader) readUserMemberships(ctx context.Context, userIDs []string) (map[string]map[string][]string, error) {
	const q = `
SELECT m.user_id, m.segment_id::text, s.visibility
FROM audience_segment_members m
JOIN audience_segments s ON s.id = m.segment_id
WHERE m.user_id = ANY($1) AND (m.expires_at IS NULL OR m.expires_at > now())`
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, q, pq.Array(userIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grouped := map[string]map[string][]string{}
	for rows.Next() {
		var userID, segmentID, visibility string
		if err := rows.Scan(&userID, &segmentID, &visibility); err != nil {
			return nil, err
		}
		if grouped[userID] == nil {
			grouped[userID] = map[string][]string{}
		}
		grouped[userID][visibility] = append(grouped[userID][visibility], segmentID)
	}
	return grouped, rows.Err()
}

// readSegmentUsers lists the current (non-expired) member user ids of a segment.
func (p *Preloader) readSegmentUsers(ctx context.Context, segmentID string) ([]string, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT user_id FROM audience_segment_members
WHERE segment_id = $1::uuid AND (expires_at IS NULL OR expires_at > now())`, segmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// writeUsers SETs each requested user's (user,visibility) keys from grouped, and
// negative-caches ("[]") any visibility with no current membership so a removed
// user stops matching. Bounded: 2 keys per user.
func (p *Preloader) writeUsers(ctx context.Context, requested []string, grouped map[string]map[string][]string) (written, tombstoned int) {
	for _, userID := range requested {
		byVis := grouped[userID]
		for _, vis := range allVisibilities {
			key := redisKey(userID, vis)
			segs := byVis[vis]
			val := "[]"
			if len(segs) > 0 {
				b, _ := json.Marshal(segs)
				val = string(b)
			}
			if err := p.l2.Set(ctx, key, val, p.ttl); err != nil {
				p.log.Debug("audience delta set failed", "key", key, "error", err)
				continue
			}
			if len(segs) > 0 {
				written++
			} else {
				tombstoned++
			}
		}
	}
	return written, tombstoned
}

func redisKey(userID, visibility string) string {
	return "audience:user:" + userID + ":" + visibility
}

// setRedisKey is the Redis SET key the append-based writer maintains and the bid
// path reads (must match cmd/pipeline's setKey).
func setRedisKey(userID, visibility string) string {
	return "audience:set:" + userID + ":" + visibility
}

// prevKeysRedisKey holds the set of (user,visibility) keys written by the most
// recent preload cycle, shared across all SSP replicas so any pod can compute
// the tombstone diff (see preloadOnce). One key, JSON array of key strings.
const prevKeysRedisKey = "audience:preload:prevkeys"

// loadPrevKeys reads the shared "written last cycle" key set. A miss/decode
// error yields an empty set — tombstoning then falls back to the local
// prevKeys union and the TTL ceiling.
func (p *Preloader) loadPrevKeys(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	val, ok, err := p.l2.Get(ctx, prevKeysRedisKey)
	if err != nil || !ok || val == "" {
		return out
	}
	var keys []string
	if err := json.Unmarshal([]byte(val), &keys); err != nil {
		return out
	}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// keysToTombstone returns the keys that carried memberships last cycle (prev)
// but none now (current) — the set to overwrite with the negative-cache "[]".
// Pure, so the multi-pod prev-set semantics (a pod that never wrote a key still
// tombstones it when it learns the key from the shared prevKeys set) are
// unit-testable without a Postgres/Redis round-trip.
func keysToTombstone(prev, current map[string]bool) []string {
	var out []string
	for key := range prev {
		if !current[key] {
			out = append(out, key)
		}
	}
	return out
}

// savePrevKeys publishes the current key set for the next cycle's diff. No
// expiry (the set is authoritative until the next preload overwrites it).
// Best-effort: a failure just degrades to per-pod prevKeys.
func (p *Preloader) savePrevKeys(ctx context.Context, current map[string]bool) {
	keys := make([]string, 0, len(current))
	for k := range current {
		keys = append(keys, k)
	}
	payload, _ := json.Marshal(keys)
	if err := p.l2.Set(ctx, prevKeysRedisKey, string(payload), 0); err != nil {
		p.log.Debug("audience preload prevkeys save failed", "error", err)
	}
}
