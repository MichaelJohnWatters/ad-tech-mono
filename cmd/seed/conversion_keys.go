package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// SeedAdvertiserConversionKeys mints one per-advertiser hmac_conversion signing
// key (G7) for every advertiser account, so the local stack runs the same
// per-advertiser conversion posture as prod: with
// tracker.conversion_strict_advertiser_key on, a conversion billed to an
// advertiser validates ONLY against that advertiser's key, not the shared
// platform key.
//
// The value is the deterministic dev key adserving.DevConversionKey(accountID) —
// the same value the harness / simulator / demoadv sign conversion postbacks
// with, so legit S2S conversions settle under strict without any key exchange.
// Real deployments issue a random, secret key per advertiser via
// POST /v1/api/conversion-key (this seeded dev value is for local prod-parity).
//
// Idempotent: skips an advertiser that already has an active hmac_conversion row
// (never overwrites an operator-rotated key), and re-seeding adds keys only for
// advertisers created since the last run.
func (in *inserter) SeedAdvertiserConversionKeys(ctx context.Context) error {
	rows, err := in.db.QueryContext(ctx, `SELECT id::text FROM accounts WHERE type = 'advertiser'`)
	if err != nil {
		return fmt.Errorf("list advertiser accounts: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		return fmt.Errorf("seed conversion keys: encryption key: %w", err)
	}

	minted := 0
	for _, accountID := range ids {
		var existing string
		err := in.db.QueryRowContext(ctx,
			`SELECT id FROM secrets WHERE purpose = 'hmac_conversion' AND account_id = $1::uuid AND status = 'active' LIMIT 1`,
			accountID).Scan(&existing)
		if err == nil {
			continue // already has one
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("check existing conversion key for %s: %w", accountID, err)
		}
		storedValue, err := cipher.Encrypt(adserving.DevConversionKey(accountID))
		if err != nil {
			return fmt.Errorf("seed conversion key encrypt for %s: %w", accountID, err)
		}
		if _, err := in.db.ExecContext(ctx,
			`INSERT INTO secrets (name, value, purpose, owner, account_id, status, created_at, updated_at)
			 VALUES ($1, $2, 'hmac_conversion', 'platform', $3::uuid, 'active', now(), now())`,
			"dev-conv-key-"+accountID, storedValue, accountID); err != nil {
			return fmt.Errorf("seed conversion key for %s: %w", accountID, err)
		}
		minted++
	}
	in.log.Info("seeded per-advertiser conversion keys", "advertisers", len(ids), "minted_now", minted,
		"value", "(dev deterministic — rotate to a random key in prod)")
	return nil
}
