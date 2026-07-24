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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
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
		keys.CacheWarm.SecretsPollInterval.Get(cfg),
		keys.CacheWarm.PollInterval.Get(cfg),
	)

	loader := pickLoader(cfg, log, serviceName)

	// Optional bus — if NATS is reachable, rotations propagate sub-second.
	// Without it the 30s poll still keeps the cache eventually consistent.
	natsURL := keys.NATS.URL.Get(cfg)
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

// LookupActiveByPurpose returns the active (canonical) secret for a given
// purpose, e.g. the JWT signing key (purpose=jwt_signing). When several are
// active it returns the first — callers that rotate should use distinct
// names and LookupActiveByName, or accept "any active key for this purpose".
func (c *Cache) LookupActiveByPurpose(purpose string) (Secret, bool) {
	if c.Cache == nil {
		return Secret{}, false
	}
	for _, s := range c.Cache.All() {
		if s.Purpose == purpose && s.Status == StatusActive {
			return s, true
		}
	}
	return Secret{}, false
}

// NonRevokedByPurpose returns every secret for a purpose that a VALIDATOR should
// still accept: status active OR rotating (not revoked), and not past its
// expires_at. This is the OVERLAP set — during a rotation both the new (active)
// and old (rotating) keys validate, so signatures already in flight don't break
// the instant a key rotates. Used for HMAC keys (tracker pixel URLs) where the
// validator must try each key VALUE, unlike LookupByValue (which matches a
// presented secret). Returns newest-rotated-first is not guaranteed; callers try
// all. Empty when the cache is unset.
func (c *Cache) NonRevokedByPurpose(purpose string) []Secret {
	if c.Cache == nil {
		return nil
	}
	now := time.Now()
	var out []Secret
	for _, s := range c.Cache.All() {
		if s.Purpose != purpose || s.Status == StatusRevoked {
			continue
		}
		if s.ExpiresAt != nil && s.ExpiresAt.Before(now) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// WatchActive keeps a signer in sync with the store's ACTIVE secret for a
// purpose: it calls set(value) immediately (boot) and again every interval,
// until ctx is done. Signer services (adserver, publisher-adserver) use it to
// sign with the current hmac_tracker key — so when an operator rotates (adds a
// new active key), newly-signed URLs follow within one interval, while the
// tracker's overlap set keeps accepting URLs signed with the predecessor. A
// missing/empty active secret is left alone (set is not called), so signing
// falls back to whatever default the signer already holds.
func (c *Cache) WatchActive(ctx context.Context, purpose string, interval time.Duration, set func(string)) {
	apply := func() {
		if s, ok := c.LookupActiveByPurpose(purpose); ok && s.Value != "" {
			set(s.Value)
		}
	}
	apply()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				apply()
			}
		}
	}()
}

// pickLoader returns a PostgresLoader keyed by dbURL. The loader is
// self-healing: it lazily opens the connection on first LoadAll and
// re-opens on every poll if the existing connection is dead. So even
// if Postgres is down at boot, the next 30s tick reconnects without
// any operator action. If dbURL is empty we still return a loader,
// just one whose LoadAll always errors-and-recovers — keeps the cache
// behaviour uniform across configurations.
func pickLoader(cfg *config.Config, log *slog.Logger, serviceName string) warm.Loader[Secret] {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		log.Warn("secrets cache: database.url not set, cache will stay empty until configured")
	}

	// At-rest decryption key. A malformed key (set but invalid) is a hard
	// misconfig — log ERROR and leave the cipher disabled so encrypted
	// rows fail to decrypt and surface the problem via /readyz, rather
	// than the service silently treating ciphertext as the credential.
	cipher, err := NewCipherFromEnv()
	if err != nil {
		log.Error("secrets cache: invalid encryption key, secrets will not decrypt", "error", err)
		cipher = &Cipher{}
	}
	if !cipher.Enabled() {
		log.Warn("secrets cache: " + EncryptionKeyEnv + " not set, secret values stored/read as plaintext (dev mode)")
	}

	return &PostgresLoader{
		DBURL:       dbURL,
		ServiceName: serviceName,
		Log:         log,
		Cipher:      cipher,
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
