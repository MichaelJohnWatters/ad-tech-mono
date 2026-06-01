// Package cached is a Redis-backed L2 cache wrapper for audience segment
// lookups. It satisfies pkg/audience/store.Lookup and delegates to a
// fallback Lookup (typically the Postgres-direct implementation) on
// cache miss.
//
// Why this exists: the DSP bid handler used to call Postgres directly
// per bid request to fetch dsp_private segments for the user. Under
// concurrent load (drain runs, real traffic spikes) the shared connection
// pool queued queries past the inherited 100ms bid_timeout — the bid
// context cancelled the query, the bid path returned no-bid, and every
// auction in the burst came back empty.
//
// With this wrapper, the user→segments mapping lives in Redis with a
// short TTL. The hot path is ~0.5ms (Redis GET); the Postgres roundtrip
// only happens on cache miss (first lookup per user, or after TTL
// expiry). Misses are also cached (as the empty list) so a user with no
// segments doesn't repeatedly hit Postgres.
//
// Failure modes — all designed to keep the bid path moving:
//   - Redis unreachable on GET → fall through to Postgres immediately.
//   - Postgres slow/errored → return whatever we have (empty list), log
//     debug. Bid proceeds with public-only segments.
//   - Redis unreachable on SET (populate) → log debug, lookup returned
//     successfully via Postgres, just no cache benefit on next call.
package cached

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

// Store is the cached wrapper. Concurrency-safe via the underlying
// L2Cache and Lookup implementations.
type Store struct {
	base store.Lookup
	l2   cache.L2Cache
	ttl  time.Duration
	log  *slog.Logger
}

// New returns a Store that consults the L2 cache before falling back to
// base. ttl is the time-to-live for cached entries; pick something short
// enough that segment membership changes propagate (5 minutes is the
// rough recommendation — segment writes also publish a NATS invalidate
// for explicit cache busting on known users).
func New(base store.Lookup, l2 cache.L2Cache, ttl time.Duration, log *slog.Logger) *Store {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Store{base: base, l2: l2, ttl: ttl, log: log}
}

// SegmentsForUser is the public-segments lookup. SSP-side use.
func (s *Store) SegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return s.lookup(ctx, userID, "public", s.base.SegmentsForUser)
}

// DSPSegmentsForUser is the dsp_private segments lookup. DSP-side use.
func (s *Store) DSPSegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return s.lookup(ctx, userID, "dsp_private", s.base.DSPSegmentsForUser)
}

func (s *Store) lookup(
	ctx context.Context,
	userID string,
	visibility string,
	fallback func(context.Context, string) ([]string, error),
) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	key := "audience:user:" + userID + ":" + visibility

	// L2 read — best effort. Errors fall through to Postgres rather than
	// failing the lookup, since a Redis blip should never break bidding.
	if val, ok, err := s.l2.Get(ctx, key); err == nil && ok {
		var out []string
		if val == "" {
			return nil, nil
		}
		if err := json.Unmarshal([]byte(val), &out); err == nil {
			return out, nil
		}
		// JSON garbage in cache — invalidate the key and fall through.
		_ = s.l2.Delete(ctx, key)
	}

	// Cache miss (or Redis error) → Postgres fallback. We honour the
	// caller's context here; the caller is responsible for setting a
	// tight timeout if they don't want the Postgres call to eat into
	// their bid budget.
	segments, err := fallback(ctx, userID)
	if err != nil {
		return nil, err
	}

	// Populate the cache. Empty result is cached too — that's the whole
	// point of "negative caching": prevent repeated misses for users
	// with no segments from re-hitting Postgres.
	//
	// Background goroutine with a fresh context so a cancelled bid
	// context doesn't prevent the populate. Errors are logged at debug
	// (cache miss next time isn't catastrophic).
	payload, _ := json.Marshal(segments)
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		if err := s.l2.Set(bgCtx, key, string(payload), s.ttl); err != nil {
			s.log.Debug("audience cache populate failed", "key", key, "error", err)
		}
	}()

	return segments, nil
}
