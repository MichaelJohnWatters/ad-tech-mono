// Package warm is the generic warm-cache primitive for hot-path reads.
//
// A warm cache holds the full set of an entity (campaigns, creatives,
// placements, deals, etc.) in process so bid/serve handlers never hit
// Postgres on the request path. Freshness is maintained by:
//
//  1. Synchronous initial load on Start — boot fails fast if Postgres is down
//  2. Periodic poll on a clock.Clock ticker (default 30s, per-entity tunable
//     via cache.warm.{entity}.poll_interval in the config manager)
//  3. NATS pub/sub on `adtech.cache.invalidate.{entity}` subjects for
//     near-real-time updates when something changes via Gateway
//
// Reads are lock-free via atomic.Pointer snapshots — a refresh swaps the
// whole snapshot in one CAS, never partially updates. Callers get a stable
// view for the duration of a request.
package warm

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/podid"
)

// Loader is the source-of-truth reader for an entity. Implementations
// live in pkg/store/postgres and are typically a single SELECT join.
type Loader[T any] interface {
	// LoadAll returns the current full set of T from the source of truth.
	LoadAll(ctx context.Context) ([]T, error)
	// KeyOf returns the unique identifier used for ByID lookups.
	KeyOf(T) string
}

// SingleLoader is an OPTIONAL capability: a loader that can fetch ONE entity
// by key. When present, a targeted invalidate (an event carrying the changed
// entity's id) updates just that one cache entry instead of reloading the
// entire set from the source of truth. That turns the per-edit cost from
// O(all rows) — a full DB scan on every campaign edit, the model's main
// scale wall — into O(1 row). Loaders that can't do single lookups simply
// don't implement it and fall back to a full reload.
type SingleLoader[T any] interface {
	// LoadOne returns the entity for key. found=false means it no longer
	// belongs in the cache (deleted, or filtered out — e.g. a campaign that
	// left 'live'), so the cache evicts it. A non-nil error falls back to a
	// full reload rather than risk a stale/partial single update.
	LoadOne(ctx context.Context, key string) (value T, found bool, err error)
}

// Config wires a warm cache to its dependencies. All fields are required
// except Bus, which may be nil to disable NATS-driven invalidation
// (polling alone still keeps the cache fresh).
type Config[T any] struct {
	Name              string          // entity name for logs and metrics, e.g. "campaigns"
	Loader            Loader[T]       // source-of-truth reader
	Clock             clock.Clock     // injected for deterministic tests
	Bus               events.EventBus // optional; nil = poll-only mode
	InvalidateSubject string          // NATS subject for cross-pod invalidate (ignored if Bus is nil)
	PollInterval      time.Duration   // tick rate for periodic refresh
	Log               *slog.Logger
	// OnRefresh, if non-nil, is called after every successful refresh with the
	// freshly loaded set. Used to sync derived state into another structure
	// (e.g. populate a separate ContractStore from a contracts warm cache).
	// Errors are logged; they do not prevent the snapshot from being installed.
	OnRefresh func(ctx context.Context, rows []T)
	// ResubscribeInterval is how often to retry the NATS invalidate
	// subscription after an initial failure (e.g. JetStream briefly
	// unavailable at boot). 0 → defaultResubscribeInterval. The poll loop
	// keeps the cache fresh meanwhile; re-subscribing just restores sub-second
	// invalidation instead of staying poll-only until the pod restarts.
	ResubscribeInterval time.Duration
}

// defaultResubscribeInterval is the fallback retry cadence for a failed
// invalidate subscription. ~JetStream-recovery timescale after a cluster blip.
const defaultResubscribeInterval = 10 * time.Second

// Cache is a snapshot-backed in-memory cache of T keyed by string ID.
// The zero value is not usable — construct via New and call Start.
type Cache[T any] struct {
	cfg      Config[T]
	snapshot atomic.Pointer[snapshot[T]]
	refresh  chan struct{}
	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan struct{}

	mu        sync.Mutex
	lastLoad  time.Time
	lastErr   error
	loadCount uint64
}

type snapshot[T any] struct {
	byID map[string]T
	all  []T
}

// New constructs a warm cache. Call Start to begin the initial load + poll loop.
func New[T any](cfg Config[T]) *Cache[T] {
	c := &Cache[T]{
		cfg:     cfg,
		refresh: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	c.snapshot.Store(&snapshot[T]{byID: map[string]T{}, all: nil})
	return c
}

// Start performs a synchronous initial load and then runs the refresh loop
// in a goroutine. Returns the load error if the first read fails so callers
// can fail fast at boot. Subsequent refresh errors are logged, not returned.
func (c *Cache[T]) Start(ctx context.Context) error {
	if err := c.refreshNow(ctx); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	if c.cfg.Bus != nil && c.cfg.InvalidateSubject != "" {
		// Group must be per-REPLICA, not per-service: warm caches need broadcast
		// semantics (every pod refreshes its own snapshot on invalidate). If
		// multiple pods of the same service shared the consumer name, they'd
		// load-balance the invalidates — only one pod would refresh per
		// message, leaving the others with stale caches and silently
		// breaking cache coherence across the deployment.
		//
		// Use podid.Replica() (the hostname / pod name), NOT POD_NAME: POD_NAME
		// is pinned to a stable, SHARED value per service (e.g. "ssp-0") for the
		// config system, so it is identical across replicas — using it here
		// collapsed all replicas into one queue group and only one reloaded.
		group := c.cfg.Name + "-cache-" + podid.Replica()
		err := c.cfg.Bus.Subscribe(ctx, c.cfg.InvalidateSubject, group, c.onInvalidate)
		if err != nil {
			// Poll-only for now, but self-heal: a transient failure at boot
			// (JetStream unavailable during a cluster restart) shouldn't leave
			// the cache stuck on the poll interval until the pod is bounced.
			c.cfg.Log.Warn("warm cache nats subscribe failed (poll-only until re-subscribe succeeds)",
				"cache", c.cfg.Name, "subject", c.cfg.InvalidateSubject, "error", err)
			go c.resubscribeLoop(ctx, group)
		}
	}

	go c.loop(ctx)
	return nil
}

// Stop cancels the refresh loop. Safe to call multiple times.
func (c *Cache[T]) Stop() {
	c.stopOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		<-c.done
	})
}

// ByID returns the entity with the given key, if present.
func (c *Cache[T]) ByID(id string) (T, bool) {
	s := c.snapshot.Load()
	v, ok := s.byID[id]
	return v, ok
}

// All returns the current snapshot as a slice. The returned slice MUST
// NOT be modified — it is shared with all other readers. Iterate, don't write.
func (c *Cache[T]) All() []T {
	return c.snapshot.Load().all
}

// Len returns the number of entries in the current snapshot.
func (c *Cache[T]) Len() int {
	return len(c.snapshot.Load().all)
}

// LastLoaded reports when the cache last successfully refreshed and the
// last error (if any). Used by /readyz and admin/debug endpoints.
func (c *Cache[T]) LastLoaded() (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastLoad, c.lastErr
}

// Trigger requests an out-of-band refresh. Non-blocking; if a refresh is
// already queued the call is a no-op. Used by NATS invalidate handlers.
func (c *Cache[T]) Trigger() {
	select {
	case c.refresh <- struct{}{}:
	default:
	}
}

// Refresh performs a synchronous reload from the Loader. Returns the count
// of entries now in the snapshot. Used by the debug /debug/cache/refresh
// endpoint so tests and ops tooling can force an immediate refresh without
// waiting for the next poll tick or a NATS invalidate.
//
// Concurrency: calls Loader.LoadAll directly — the poll-loop refresh on the
// next tick will simply overwrite this snapshot with the latest data. No
// lock contention since the snapshot is atomic.Pointer-backed.
func (c *Cache[T]) Refresh(ctx context.Context) (int, error) {
	if err := c.refreshNow(ctx); err != nil {
		return 0, err
	}
	return c.Len(), nil
}

func (c *Cache[T]) loop(ctx context.Context) {
	defer close(c.done)
	ticker := c.cfg.Clock.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.refreshNow(ctx); err != nil {
				c.cfg.Log.Warn("warm cache refresh failed", "cache", c.cfg.Name, "error", err)
			}
		case <-c.refresh:
			if err := c.refreshNow(ctx); err != nil {
				c.cfg.Log.Warn("warm cache triggered refresh failed", "cache", c.cfg.Name, "error", err)
			}
		}
	}
}

// resubscribeLoop retries the invalidate subscription after an initial failure
// until it succeeds or ctx is canceled. Fire-and-forget: it exits on the same
// ctx the poll loop uses, so Stop() (via cancel) unwinds it. On success it
// triggers one refresh so any invalidates missed while poll-only are caught up.
func (c *Cache[T]) resubscribeLoop(ctx context.Context, group string) {
	interval := c.cfg.ResubscribeInterval
	if interval <= 0 {
		interval = defaultResubscribeInterval
	}
	ticker := c.cfg.Clock.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.cfg.Bus.Subscribe(ctx, c.cfg.InvalidateSubject, group, c.onInvalidate); err != nil {
				c.cfg.Log.Debug("warm cache nats re-subscribe retry failed",
					"cache", c.cfg.Name, "error", err)
				continue
			}
			c.cfg.Log.Info("warm cache nats re-subscribe succeeded",
				"cache", c.cfg.Name, "subject", c.cfg.InvalidateSubject)
			c.Trigger() // catch up on anything missed while poll-only
			return
		}
	}
}

func (c *Cache[T]) refreshNow(ctx context.Context) error {
	rows, err := c.cfg.Loader.LoadAll(ctx)
	c.mu.Lock()
	c.loadCount++
	c.lastErr = err
	if err == nil {
		c.lastLoad = c.cfg.Clock.Now()
	}
	c.mu.Unlock()

	if err != nil {
		return err
	}

	snap := &snapshot[T]{byID: make(map[string]T, len(rows)), all: rows}
	for _, row := range rows {
		snap.byID[c.cfg.Loader.KeyOf(row)] = row
	}
	c.snapshot.Store(snap)
	if c.cfg.OnRefresh != nil {
		c.cfg.OnRefresh(ctx, rows)
	}
	c.cfg.Log.Debug("warm cache refreshed", "cache", c.cfg.Name, "count", len(rows))
	return nil
}

// onInvalidate is the NATS subscribe handler. Any message on the invalidate
// subject triggers a refresh — payload is ignored (we always reload the
// full set, not partial; partial updates would race with concurrent writes).
// PublishInvalidate broadcasts this cache's invalidate subject so every
// replica (per-pod consumer groups) refreshes — the debug refresh endpoint
// uses it to escape its own load-balanced single-pod reach. No-op without a
// bus or subject (poll-only caches).
func (c *Cache[T]) PublishInvalidate(ctx context.Context) error {
	if c.cfg.Bus == nil || c.cfg.InvalidateSubject == "" {
		return nil
	}
	payload, err := json.Marshal(events.CacheInvalidateEvent{
		SchemaVersion: events.CurrentSchemaVersion,
		ResourceType:  c.cfg.Name,
		Action:        "refresh-broadcast",
	})
	if err != nil {
		return err
	}
	return c.cfg.Bus.Publish(ctx, c.cfg.InvalidateSubject, payload)
}

func (c *Cache[T]) onInvalidate(ctx context.Context, msg *events.Message) error {
	// Targeted path: if the event names a single changed id AND the loader
	// can fetch one AND there's no OnRefresh hook (which needs the full set),
	// update just that entry — no DB full-scan. Otherwise reload everything.
	if id := invalidateID(msg.Data); id != "" && c.cfg.OnRefresh == nil {
		if sl, ok := c.cfg.Loader.(SingleLoader[T]); ok {
			if err := c.applyOne(ctx, sl, id); err == nil {
				c.cfg.Log.Info("warm cache targeted update", "cache", c.cfg.Name, "id", id)
				_ = msg.Ack()
				return nil
			}
			// applyOne failed → fall through to a full reload (safe default).
			c.cfg.Log.Warn("warm cache targeted update failed, full reload", "cache", c.cfg.Name, "id", id)
		}
	}
	c.cfg.Log.Info("warm cache invalidate received, reloading", "cache", c.cfg.Name, "subject", msg.Subject)
	c.Trigger()
	_ = msg.Ack()
	return nil
}

// invalidateID extracts a single-entity id from an invalidate payload
// ({"id":"..."} — the dsp-mgmt shape) if present. Empty => "reload all"
// (the refresh-broadcast / poll shapes carry no id).
func invalidateID(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	return m.ID
}

// applyOne upserts (or evicts) a single entry into a fresh snapshot,
// preserving the lock-free copy-on-write model. The DB cost is one row
// (LoadOne), not the whole set; the snapshot rebuild is in-process.
func (c *Cache[T]) applyOne(ctx context.Context, sl SingleLoader[T], id string) error {
	val, found, err := sl.LoadOne(ctx, id)
	if err != nil {
		return err
	}
	cur := c.snapshot.Load()
	next := &snapshot[T]{byID: make(map[string]T, len(cur.byID)+1)}
	for k, v := range cur.byID {
		if k == id {
			continue // drop the old copy; re-added below if still present
		}
		next.byID[k] = v
	}
	if found {
		next.byID[id] = val
	}
	next.all = make([]T, 0, len(next.byID))
	for _, v := range next.byID {
		next.all = append(next.all, v)
	}
	c.snapshot.Store(next)
	return nil
}
