package secrets

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

// Cache wraps the generic warm cache with secrets-specific lookup helpers.
// Holds an atomic.Bool readiness flag so /readyz can gate traffic until
// the first successful load. Reads from the snapshot are lock-free and
// microsecond-scale — same hot-path cost as every other warm cache in
// the platform.
type Cache struct {
	*warm.Cache[Secret]
	loaded atomic.Bool
}

// Start opens its own bus + Postgres connection (same nil-tolerant
// pattern other services use) and kicks off the warm cache. Returns
// even if Postgres is unreachable — the poll loop retries every tick
// until it succeeds. /readyz callers should use Ready() to gate.
//
// Caller owns the returned *Cache lifecycle and should defer Stop().
func Start(ctx context.Context, cfg *config.Config, clk clock.Clock, log *slog.Logger, serviceName string) *Cache {
	c := &Cache{}

	pollInterval := firstNonZeroDuration(
		cfg.GetDuration("cache.warm.secrets.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 30*time.Second),
	)

	loader := pickLoader(cfg, log, serviceName)

	// Optional bus — if NATS is reachable, rotations propagate sub-second.
	// Without it the 30s poll still keeps the cache eventually consistent.
	natsURL := cfg.Get("nats.url", routes.DefaultNATSURL)
	var bus events.EventBus
	if b, err := natsbus.New(natsURL, serviceName+"-secrets", log); err == nil {
		bus = b
	} else {
		log.Warn("secrets cache: NATS unavailable, will rely on 30s poll for rotation propagation", "error", err)
	}

	inner := warm.New(warm.Config[Secret]{
		Name:              "secrets",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateSecrets,
		PollInterval:      pollInterval,
		Log:               log,
		OnRefresh: func(_ context.Context, _ []Secret) {
			c.loaded.Store(true)
		},
	})
	c.Cache = inner

	// Synchronous initial load attempt. If Postgres is down, the warm
	// cache logs a warning and continues empty; subsequent ticks retry.
	// We deliberately do NOT bubble the error — matches the existing
	// pattern used by every other warm-cache caller in the platform.
	if err := inner.Start(ctx); err != nil {
		log.Warn("secrets cache initial load failed (will retry on poll)", "error", err)
	}

	return c
}

// Ready reports whether the cache has completed at least one successful
// load. Use as a /readyz check so the service doesn't accept auth-
// required traffic before secrets are available.
func (c *Cache) Ready() error {
	if c.loaded.Load() {
		return nil
	}
	return errors.New("secrets cache not yet loaded")
}

// LookupByValue scans the snapshot for a secret whose value matches the
// presented credential AND is currently acceptable (active or rotating,
// within expiry). Returns (Secret{}, false) on no match.
//
// O(n) over the cache, where n is the number of secrets this service
// loads (typically <50). For hot-path auth middleware, the wall-clock
// cost is dominated by the http handler overhead, not the scan.
func (c *Cache) LookupByValue(value string, now time.Time) (Secret, bool) {
	if c.Cache == nil {
		return Secret{}, false
	}
	for _, s := range c.Cache.All() {
		if s.Value == value && s.IsAcceptable(now) {
			return s, true
		}
	}
	return Secret{}, false
}

// LookupActiveByName returns the canonical (status=active) secret for a
// given name. Used by signing-side code (gateway issues a JWT signed
// with the active key) — validators should use LookupByValue instead to
// also accept rotating predecessors.
func (c *Cache) LookupActiveByName(name string) (Secret, bool) {
	if c.Cache == nil {
		return Secret{}, false
	}
	for _, s := range c.Cache.All() {
		if s.Name == name && s.Status == StatusActive {
			return s, true
		}
	}
	return Secret{}, false
}

// pickLoader returns a PostgresLoader keyed by dbURL. The loader is
// self-healing: it lazily opens the connection on first LoadAll and
// re-opens on every poll if the existing connection is dead. So even
// if Postgres is down at boot, the next 30s tick reconnects without
// any operator action. If dbURL is empty we still return a loader,
// just one whose LoadAll always errors-and-recovers — keeps the cache
// behaviour uniform across configurations.
func pickLoader(cfg *config.Config, log *slog.Logger, serviceName string) warm.Loader[Secret] {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("secrets cache: database.url not set, cache will stay empty until configured")
	}
	return &PostgresLoader{
		DBURL:       dbURL,
		ServiceName: serviceName,
		Log:         log,
	}
}

func firstNonZeroDuration(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 30 * time.Second
}
