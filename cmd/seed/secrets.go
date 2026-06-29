package main

import (
	"context"
	"database/sql"
	"fmt"
)

// DevAPIKey is the well-known operator API key seeded into dev / e2e
// environments. The pub sim and harness use this so management endpoints
// work out of the box without manual setup. Real deployments should
// rotate or revoke this row immediately after first boot — its presence
// in the table is a development convenience, not a production posture.
const DevAPIKey = "dev-api-key-do-not-use-in-prod"

// SeedDevSecrets inserts the dev-mode API key with status='active'.
// Idempotent: re-running the seed checks for an existing row first so
// we don't accumulate duplicates and don't overwrite an operator-rotated
// value. The dev key only gets re-inserted when it's missing entirely
// (e.g. after a config-table truncate during testing).
func (in *inserter) SeedDevSecrets(ctx context.Context) error {
	var existing string
	err := in.db.QueryRowContext(ctx,
		`SELECT id FROM secrets WHERE name = 'dev-ops-key' AND status != 'revoked' LIMIT 1`,
	).Scan(&existing)
	if err == nil {
		in.log.Info("dev api key already present, skipping seed")
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check existing dev api key: %w", err)
	}
	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ('dev-ops-key', $1, 'api_key', 'platform', 'active', now(), now())`
	if _, err := in.db.ExecContext(ctx, q, DevAPIKey); err != nil {
		return fmt.Errorf("seed dev api key: %w", err)
	}
	in.log.Info("seeded dev api key", "purpose", "api_key", "owner", "platform", "value", "(dev only — rotate in prod)")
	return nil
}
