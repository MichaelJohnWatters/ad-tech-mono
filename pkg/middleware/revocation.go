package middleware

import (
	"context"
	"log/slog"
	"strconv"
	"time"
)

// RevocationChecker reports whether a token — identified by its subject and
// issue time — has been revoked. A nil checker on the Auth middleware means
// revocation is disabled (backward-compatible default).
type RevocationChecker interface {
	IsRevoked(ctx context.Context, userID string, issuedAt time.Time) (bool, error)
}

// revocationKV is the minimal key/value surface the store needs: Get + Set with
// a TTL. cache.L2Cache satisfies it structurally, so the gateway passes its
// self-healing Redis L2 without pkg/middleware importing pkg/cache (no cycle).
type revocationKV interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
}

// RevocationStore is a Redis-backed session-revocation checkpoint. Revoking a
// user writes a cutoff timestamp; any token ISSUED before that cutoff is then
// rejected. This kills all of a user's outstanding tokens at once — the
// "log out everywhere" / stolen-token response — without per-token state, using
// the token's own `iat` claim as the version. Checkpoints carry a TTL past the
// maximum token lifetime, after which every affected token has expired anyway,
// so the keyspace self-cleans.
type RevocationStore struct {
	kv  revocationKV
	ttl time.Duration
	log *slog.Logger
}

// NewRevocationStore builds a store over kv. maxTokenLifetime should be the JWT
// expiry window (checkpoints live an hour longer so they outlast any token they
// could affect). A nil kv makes every operation a safe no-op.
func NewRevocationStore(kv revocationKV, maxTokenLifetime time.Duration, log *slog.Logger) *RevocationStore {
	return &RevocationStore{kv: kv, ttl: maxTokenLifetime + time.Hour, log: log}
}

func revocationKey(userID string) string { return "auth:revoked:" + userID }

// RevokeUser sets the cutoff so every token for userID issued strictly before
// `at` is rejected. Call with time.Now() to kill all current sessions (the
// caller's included). Returns the store error so the caller can surface a failed
// revoke (unlike the read path, a write that silently failed would be unsafe).
func (s *RevocationStore) RevokeUser(ctx context.Context, userID string, at time.Time) error {
	if s == nil || s.kv == nil || userID == "" {
		return nil
	}
	return s.kv.Set(ctx, revocationKey(userID), strconv.FormatInt(at.Unix(), 10), s.ttl)
}

// IsRevoked reports whether a token for userID issued at issuedAt has been
// revoked. Fail-OPEN on a store error (consistent with the platform's Redis
// posture for budgets/freq-caps): revocation is defence-in-depth on top of the
// token's own 12h expiry, so a Redis outage must not lock every user out. The
// caller logs the error.
func (s *RevocationStore) IsRevoked(ctx context.Context, userID string, issuedAt time.Time) (bool, error) {
	if s == nil || s.kv == nil || userID == "" {
		return false, nil
	}
	v, ok, err := s.kv.Get(ctx, revocationKey(userID))
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	cutoff, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return false, nil
	}
	// Strictly-before so a token minted AFTER the revoke (a fresh login in a
	// later second) is never caught by its own log-out-everywhere. The
	// sub-second tail (a token minted in the same second as the revoke) is
	// immaterial against a 12h token lifetime.
	return issuedAt.Unix() < cutoff, nil
}
