package main

import (
	"context"
	"log/slog"
	"time"

	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
)

// redisFPStore is a Redis-backed identityobserve.FPStore. It keeps the
// probabilistic fingerprint buckets in Redis sets (one per fingerprint, TTL'd)
// so the buckets stay coherent if the consumer is scaled beyond one replica.
// Best-effort: any Redis error degrades to "no link" rather than failing.
type redisFPStore struct {
	c   *cacheredis.Client
	ttl time.Duration
	log *slog.Logger
}

func newRedisFPStore(c *cacheredis.Client, ttl time.Duration, log *slog.Logger) *redisFPStore {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &redisFPStore{c: c, ttl: ttl, log: log}
}

func (s *redisFPStore) Observe(fp, id string, maxUsers int) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	key := "identity:fp:" + fp

	if member, err := s.c.SIsMember(ctx, key, id); err != nil {
		s.log.Debug("fp store SIsMember failed (skipping link)", "error", err)
		return nil
	} else if member {
		return nil // already tracked on this fingerprint
	}
	if n, err := s.c.SCard(ctx, key); err != nil {
		return nil
	} else if n >= int64(maxUsers) {
		return nil // shared IP → don't link
	}
	others, err := s.c.SMembers(ctx, key)
	if err != nil {
		return nil
	}
	// Add + refresh TTL. Not atomic with the read above, but a race only means
	// a slightly stale member list / a few extra links — deduped downstream.
	if err := s.c.SAdd(ctx, key, id); err != nil {
		s.log.Debug("fp store SAdd failed", "error", err)
	}
	_ = s.c.Expire(ctx, key, s.ttl)
	return others
}
