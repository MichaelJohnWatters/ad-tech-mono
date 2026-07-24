package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adcert"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pgp"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// SeedDevAdCertKey generates a platform ads.cert Ed25519 keypair and stores the
// PRIVATE key as the canonical ACTIVE adcert_ed25519 secret (Phase I). The
// exchange signs outbound bid requests with it and publishes the public half at
// /v1/adcert/key; rotation = add a new active row (this becomes 'rotating', then
// 'revoked') and DSPs verify the overlap keyset live. Idempotent.
func (in *inserter) SeedDevAdCertKey(ctx context.Context) error {
	var existing string
	err := in.db.QueryRowContext(ctx,
		`SELECT id FROM secrets WHERE purpose = 'adcert_ed25519' AND status != 'revoked' LIMIT 1`,
	).Scan(&existing)
	if err == nil {
		in.log.Info("adcert_ed25519 secret already present, skipping seed")
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check existing adcert key: %w", err)
	}
	_, priv, gerr := ed25519.GenerateKey(nil)
	if gerr != nil {
		return fmt.Errorf("seed adcert key: generate: %w", gerr)
	}
	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		return fmt.Errorf("seed adcert key: encryption key: %w", err)
	}
	storedValue, err := cipher.Encrypt(adcert.EncodeKey(priv))
	if err != nil {
		return fmt.Errorf("seed adcert key: encrypt: %w", err)
	}
	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ('dev-adcert-ed25519', $1, 'adcert_ed25519', 'platform', 'active', now(), now())`
	if _, err := in.db.ExecContext(ctx, q, storedValue); err != nil {
		return fmt.Errorf("seed adcert key: %w", err)
	}
	in.log.Info("seeded dev adcert_ed25519 key", "purpose", "adcert_ed25519", "owner", "platform", "value", "(dev only — rotate in prod)")
	return nil
}

// SeedDevHMACTracker inserts the current pixel-signing key as the canonical
// ACTIVE hmac_tracker secret (Phase I). Value = adserving.DefaultSigningKey (what
// the ad servers sign with today), so existing signatures still validate and the
// key becomes a real, rotatable secrets-store row: an operator rotates it by
// adding a new active row (this becomes 'rotating' during the grace window, then
// 'revoked') and the tracker accepts the overlap set live. Idempotent.
func (in *inserter) SeedDevHMACTracker(ctx context.Context) error {
	var existing string
	err := in.db.QueryRowContext(ctx,
		`SELECT id FROM secrets WHERE purpose = 'hmac_tracker' AND status != 'revoked' LIMIT 1`,
	).Scan(&existing)
	if err == nil {
		in.log.Info("hmac_tracker secret already present, skipping seed")
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check existing hmac_tracker: %w", err)
	}
	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		return fmt.Errorf("seed hmac_tracker: encryption key: %w", err)
	}
	storedValue, err := cipher.Encrypt(adserving.DefaultSigningKey)
	if err != nil {
		return fmt.Errorf("seed hmac_tracker: encrypt: %w", err)
	}
	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ('dev-hmac-tracker', $1, 'hmac_tracker', 'platform', 'active', now(), now())`
	if _, err := in.db.ExecContext(ctx, q, storedValue); err != nil {
		return fmt.Errorf("seed hmac_tracker: %w", err)
	}
	in.log.Info("seeded dev hmac_tracker key", "purpose", "hmac_tracker", "owner", "platform", "value", "(dev only — rotate in prod)")
	return nil
}

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
	// Encrypt at rest, same as the gateway create path. With no key
	// configured (dev default) this is a passthrough and the row stays
	// plaintext — the warm-cache loader decrypts symmetrically either way.
	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		return fmt.Errorf("seed dev api key: encryption key: %w", err)
	}
	storedValue, err := cipher.Encrypt(DevAPIKey)
	if err != nil {
		return fmt.Errorf("seed dev api key: encrypt: %w", err)
	}
	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ('dev-ops-key', $1, 'api_key', 'platform', 'active', now(), now())`
	if _, err := in.db.ExecContext(ctx, q, storedValue); err != nil {
		return fmt.Errorf("seed dev api key: %w", err)
	}
	in.log.Info("seeded dev api key", "purpose", "api_key", "owner", "platform", "value", "(dev only — rotate in prod)")
	return nil
}

// DevJWTSigningKey is the well-known dev JWT signing key. Its presence as an
// active jwt_signing secret flips the gateway to REAL auth (sessions
// validated, dev bypass off) — UI plan F4. The value only matters locally;
// real deployments rotate it via the secrets console.
const DevJWTSigningKey = "dev-jwt-signing-key-do-not-use-in-prod"

// SeedDevJWTSigningKey inserts the dev signing key with status='active'.
// Idempotent, same pattern as the dev API key. NOTE: the gateway reads the
// signing key at BOOT — after the very first seed, restart the gateway
// (tilt trigger gateway) to activate real auth.
func (in *inserter) SeedDevJWTSigningKey(ctx context.Context) error {
	var existing string
	err := in.db.QueryRowContext(ctx,
		`SELECT id FROM secrets WHERE purpose = 'jwt_signing' AND status = 'active' LIMIT 1`,
	).Scan(&existing)
	if err == nil {
		in.log.Info("active jwt_signing secret already present, skipping seed")
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check existing jwt signing key: %w", err)
	}
	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		return fmt.Errorf("seed jwt signing key: encryption key: %w", err)
	}
	storedValue, err := cipher.Encrypt(DevJWTSigningKey)
	if err != nil {
		return fmt.Errorf("seed jwt signing key: encrypt: %w", err)
	}
	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ('dev-jwt-signing', $1, 'jwt_signing', 'platform', 'active', now(), now())`
	if _, err := in.db.ExecContext(ctx, q, storedValue); err != nil {
		return fmt.Errorf("seed jwt signing key: %w", err)
	}
	in.log.Info("seeded dev jwt signing key — gateway runs REAL auth once restarted",
		"purpose", "jwt_signing", "logins", "admin@ / advertiser@ / publisher@ adtech.local (password: admin)")
	return nil
}

// SeedDevPGPKey generates a platform PGP keypair and stores the armored PRIVATE
// key as an active pgp_private secret (ADR 0008), so audience files providers
// encrypt to the platform public key decrypt on ingest locally. Prod supplies
// the private key via SOPS/K8s Secret instead. Idempotent: skips when an active
// pgp_private row already exists (never overwrites an operator-rotated key).
// NOTE: the gateway + pipeline read this key at BOOT — restart them after the
// first seed to activate decrypt-on-ingest.
func (in *inserter) SeedDevPGPKey(ctx context.Context) error {
	var existing string
	err := in.db.QueryRowContext(ctx,
		`SELECT id FROM secrets WHERE purpose = 'pgp_private' AND status = 'active' LIMIT 1`,
	).Scan(&existing)
	if err == nil {
		in.log.Info("active pgp_private secret already present, skipping seed")
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check existing pgp private key: %w", err)
	}
	armoredPrivate, _, fingerprint, err := pgp.Generate()
	if err != nil {
		return fmt.Errorf("seed pgp key: generate: %w", err)
	}
	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		return fmt.Errorf("seed pgp key: encryption key: %w", err)
	}
	storedValue, err := cipher.Encrypt(armoredPrivate)
	if err != nil {
		return fmt.Errorf("seed pgp key: encrypt: %w", err)
	}
	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ('pgp-ingest', $1, 'pgp_private', 'platform', 'active', now(), now())`
	if _, err := in.db.ExecContext(ctx, q, storedValue); err != nil {
		return fmt.Errorf("seed pgp key: %w", err)
	}
	in.log.Info("seeded dev pgp ingest keypair — restart gateway + pipeline to enable decrypt-on-ingest",
		"purpose", "pgp_private", "owner", "platform", "fingerprint", fingerprint)
	return nil
}
