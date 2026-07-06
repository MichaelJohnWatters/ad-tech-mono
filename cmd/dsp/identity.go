package main

import (
	"context"
	"database/sql"
	"log/slog"
	"sync/atomic"
	"time"

	audstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// identityResolver expands a user key to the identifiers linked to it in the
// identity graph. Satisfied by the preload resolver below.
type identityResolver interface {
	ResolveIdentity(ctx context.Context, id string) ([]string, error)
}

// graphLoader is the read dependency of the preload resolver — the whole graph
// as a bidirectional adjacency map. Satisfied by *postgres.Store.
type graphLoader interface {
	LoadIdentityGraph(ctx context.Context) (map[string][]string, error)
}

// openIdentityResolver builds the DSP's identity resolver when
// dsp.identity_resolution_enabled is set (off by default). Resolution is served
// from an in-memory snapshot of the graph, refreshed periodically in the
// background — the bid path never touches Postgres, so it stays QPS-safe (the
// same warm-cache pattern the campaign/placement/deal caches use). Returns
// (nil, no-op) when disabled or Postgres is unreachable at boot.
func openIdentityResolver(cfg *config.Config, log *slog.Logger) (identityResolver, func()) {
	if !cfg.GetBool("dsp.identity_resolution_enabled", false) {
		return nil, func() {}
	}
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("dsp identity resolution enabled but database.url unset; resolution disabled")
		return nil, func() {}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("dsp identity resolver open failed", "error", err)
		return nil, func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("dsp identity resolver ping failed", "error", err)
		_ = db.Close()
		return nil, func() {}
	}
	p := newPreloadIdentityResolver(postgres.NewFromDB(db), cfg.GetDuration("dsp.identity_preload_interval", 5*time.Minute), log)
	p.Start()
	log.Info("dsp identity resolution enabled (in-memory preload)")
	return p, func() { p.Stop(); _ = db.Close() }
}

// preloadIdentityResolver holds the whole identity graph as an in-memory
// adjacency snapshot, refreshed on an interval in the background. Bid-path
// resolution is a pure lock-free map read (atomic.Pointer snapshot) — no
// Postgres, no network — which is the right shape for a QPS-critical path.
// Tradeoff: the graph must fit in memory and reads are stale up to one refresh
// interval. At internet scale you'd shard or use a dedicated identity service;
// for this platform the whole graph fits comfortably.
type preloadIdentityResolver struct {
	loader   graphLoader
	interval time.Duration
	log      *slog.Logger
	snap     atomic.Pointer[map[string][]string]
	stop     chan struct{}
}

func newPreloadIdentityResolver(loader graphLoader, interval time.Duration, log *slog.Logger) *preloadIdentityResolver {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &preloadIdentityResolver{loader: loader, interval: interval, log: log, stop: make(chan struct{})}
}

// Start does an initial synchronous load (so the resolver is warm before it
// serves) then refreshes on the interval. A failed load leaves the last-good
// snapshot in place (empty on the very first failure — resolution just returns
// nothing, and the bid proceeds).
func (p *preloadIdentityResolver) Start() {
	p.refresh()
	go func() {
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.refresh()
			}
		}
	}()
}

func (p *preloadIdentityResolver) Stop() { close(p.stop) }

func (p *preloadIdentityResolver) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	adj, err := p.loader.LoadIdentityGraph(ctx)
	if err != nil {
		p.log.Error("identity graph preload failed (serving last snapshot)", "error", err)
		return
	}
	p.snap.Store(&adj)
	p.log.Debug("identity graph preloaded", "ids", len(adj))
}

// ResolveIdentity is a lock-free in-memory read of the current snapshot.
func (p *preloadIdentityResolver) ResolveIdentity(_ context.Context, id string) ([]string, error) {
	if id == "" {
		return nil, nil
	}
	if m := p.snap.Load(); m != nil {
		return (*m)[id], nil
	}
	return nil, nil
}

// dspPrivateSegments returns the DSP-private segment ids for a user. When a
// resolver is present, the user is first expanded via the identity graph so
// segments attached to a linked identifier (from another device / publisher /
// a UID2 CRM match) also apply — this is what makes UID2 addressable beyond a
// single request. Best-effort throughout: a failed resolve or per-id lookup is
// skipped so the bid still proceeds; the whole thing runs under the caller's
// (tight) deadline. maxLinked caps how many linked ids are folded in.
func dspPrivateSegments(ctx context.Context, store audstore.Lookup, resolver identityResolver, userKey string, maxLinked int, log *slog.Logger) []string {
	if store == nil || userKey == "" {
		return nil
	}
	ids := []string{userKey}
	if resolver != nil {
		linked, err := resolver.ResolveIdentity(ctx, userKey)
		if err != nil {
			log.Debug("identity resolve degraded (bid proceeds without linked segments)", "user_key", userKey, "error", err)
		} else {
			for _, id := range linked {
				if len(ids) > maxLinked { // ids already holds userKey, so this caps linked adds
					break
				}
				ids = append(ids, id)
			}
		}
	}
	seen := make(map[string]bool)
	var out []string
	for _, id := range ids {
		segs, err := store.DSPSegmentsForUser(ctx, id)
		if err != nil {
			log.Debug("dsp private segment lookup degraded", "id", id, "error", err)
			continue
		}
		for _, s := range segs {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}
