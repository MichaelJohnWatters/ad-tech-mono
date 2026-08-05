package main

import (
	"context"
	"fmt"
)

// SeedAdvertiserBalances grants every advertiser account a starting prepay
// balance so the demo world bids out of the box under the money loop (the
// DSP's balance gate fails CLOSED for accounts with no balance row — a real
// customer tops up through the portal; seeded demo advertisers get this
// operator grant instead).
//
// Ledger-honest: the grant is written exactly like a real topup — a topups
// row (idempotency_key 'seed-initial-grant' makes re-seeding a no-op), the
// double-entry ledger pair, and the advertiser_balances upsert — so
// balance == sum(ledger) still holds for seeded accounts.
func (in *inserter) SeedAdvertiserBalances(ctx context.Context, amount float64) error {
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

	granted := 0
	for _, accountID := range ids {
		// The overspend canary gets its own deliberately-tiny grant
		// (SeedOverspendCanary) — the uniform demo grant would defeat it.
		if accountID == DeriveID("account", canaryAccountKey) {
			continue
		}
		if err := in.grantBalance(ctx, accountID, amount, "seed-initial-grant"); err != nil {
			return err
		}
		granted++
	}
	_ = granted
	return nil
}

// grantBalance is one ledger-honest operator grant: topup row (idempotent on
// key), double-entry ledger pair, advertiser_balances upsert. Extracted so
// the canary's tiny grant uses the identical money path as the demo grants.
func (in *inserter) grantBalance(ctx context.Context, accountID string, amount float64, idemKey string) error {
	{
		tx, err := in.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
			tx.Rollback()
			return fmt.Errorf("set tenant: %w", err)
		}
		var topupID string
		err = tx.QueryRowContext(ctx,
			`INSERT INTO topups (account_id, amount, currency, status, payment_method, idempotency_key)
			 VALUES ($1::uuid, $2, 'USD', 'succeeded', 'seed', $3)
			 ON CONFLICT (account_id, idempotency_key) DO NOTHING RETURNING id::text`,
			accountID, amount, idemKey).Scan(&topupID)
		if err != nil { // sql.ErrNoRows = already granted (re-seed)
			tx.Rollback()
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (account_code, entry_type, amount, currency, reference_type, reference_id)
			 VALUES ('platform:cash', 'debit', $1, 'USD', 'topup', $2),
			        ('advertiser:' || $3 || ':balance', 'credit', $1, 'USD', 'topup', $2)`,
			amount, topupID, accountID); err != nil {
			tx.Rollback()
			return fmt.Errorf("seed grant ledger pair: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
			 VALUES ($1::uuid, $2, 'USD', now())
			 ON CONFLICT (account_id) DO UPDATE
			   SET balance = advertiser_balances.balance + $2, updated_at = now()`,
			accountID, amount); err != nil {
			tx.Rollback()
			return fmt.Errorf("seed grant balance: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
