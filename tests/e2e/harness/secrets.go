//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"
)

// DevAPIKey is the seed-time well-known API key (matches
// cmd/seed/secrets.go const). E2E tests use it for management-endpoint
// auth without needing to mint their own.
const DevAPIKey = "dev-api-key-do-not-use-in-prod"

// InsertAPIKey writes a secrets row directly via SQL. The caller picks
// the value (so tests can use predictable strings), name, and status —
// status="rotating" or "revoked" lets tests assert grace-window /
// rejection behaviour.
//
// Triggers the NATS invalidate so service warm caches re-load quickly
// rather than waiting up to 30s for their poll.
func (h *Harness) InsertAPIKey(t *testing.T, name, value, status string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ($1, $2, 'api_key', 'platform', $3, now(), now())`
	if _, err := h.DB.ExecContext(ctx, q, name, value, status); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	// Best-effort: ask the SSP + DSP to refresh their warm caches now so
	// the next request sees the new key without waiting for the poll tick.
	// Failure here just means the test polls a bit longer.
	h.RefreshCache(t, h.URLs.SSP)
	h.RefreshCache(t, h.URLs.DSP)
}

// RevokeAPIKey flips a secret to status=revoked by name. Use to test
// rejection after an operator revokes a credential.
func (h *Harness) RevokeAPIKey(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := h.DB.ExecContext(ctx,
		`UPDATE secrets SET status='revoked', updated_at=now() WHERE name=$1`,
		name,
	); err != nil {
		t.Fatalf("revoke api key: %v", err)
	}
	h.RefreshCache(t, h.URLs.SSP)
	h.RefreshCache(t, h.URLs.DSP)
}

// UpdateAPIKeyStatus flips a secret's status by name. The full rotation
// workflow uses this to demote the previous active key to "rotating"
// then later to "revoked", or to promote a rotating key to "active".
//
// Refreshes warm caches after the update so subsequent assertions see
// the new state without waiting for the natural poll.
func (h *Harness) UpdateAPIKeyStatus(t *testing.T, name, newStatus string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := h.DB.ExecContext(ctx,
		`UPDATE secrets SET status=$2, updated_at=now() WHERE name=$1`,
		name, newStatus,
	); err != nil {
		t.Fatalf("update api key status: %v", err)
	}
	h.RefreshCache(t, h.URLs.SSP)
	h.RefreshCache(t, h.URLs.DSP)
}

// InsertAPIKeyWithExpiry writes a row with an explicit expires_at — for
// testing the natural-expiry branch in Secret.IsAcceptable. Pass a
// negative duration to insert an already-expired key.
func (h *Harness) InsertAPIKeyWithExpiry(t *testing.T, name, value string, expiresIn time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, expires_at, created_at, updated_at)
VALUES ($1, $2, 'api_key', 'platform', 'active', now() + $3::interval, now(), now())`
	// Postgres interval cast from a Go Duration string keeps the test
	// readable; "-1 hour" / "1 hour" both work.
	interval := expiresIn.String()
	if _, err := h.DB.ExecContext(ctx, q, name, value, interval); err != nil {
		t.Fatalf("insert api key with expiry: %v", err)
	}
	h.RefreshCache(t, h.URLs.SSP)
	h.RefreshCache(t, h.URLs.DSP)
}

// DeleteAPIKeysByName removes secrets rows with a given name. Used by
// tests to clean up after themselves so subsequent runs aren't poisoned
// by leftover test keys.
func (h *Harness) DeleteAPIKeysByName(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := h.DB.ExecContext(ctx, `DELETE FROM secrets WHERE name=$1`, name); err != nil {
		t.Fatalf("delete api keys: %v", err)
	}
}
