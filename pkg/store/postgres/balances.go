package postgres

import (
	"context"
	"fmt"
)

// AdvertiserBalance is one advertiser account's prepay balance as the DSP's
// warm cache stores it. The bid path gates on Balance minus the Redis spend
// mirror — see cmd/dsp BalanceGate.
type AdvertiserBalance struct {
	AccountID    string
	Balance      float64
	Currency     string
	PaymentTerms string
}

// BalanceLoader reads every advertiser balance row. One cheap SELECT — the
// table has one row per advertiser account (topup upserts it).
type BalanceLoader struct {
	Store *Store
}

func (l *BalanceLoader) LoadAll(ctx context.Context) ([]AdvertiserBalance, error) {
	const q = `
SELECT account_id::text, balance::float8, currency, payment_terms
FROM advertiser_balances`

	rows, err := l.Store.read.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query advertiser balances: %w", err)
	}
	defer rows.Close()

	var out []AdvertiserBalance
	for rows.Next() {
		var b AdvertiserBalance
		if err := rows.Scan(&b.AccountID, &b.Balance, &b.Currency, &b.PaymentTerms); err != nil {
			return nil, fmt.Errorf("scan advertiser balance: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (l *BalanceLoader) KeyOf(b AdvertiserBalance) string { return b.AccountID }

// BalanceStore is the spend-drawdown writer used by the billing sink
// (cmd/reporting). DebitSpend applies one realized spend event to the
// advertiser's balance, idempotently:
//
//   - one tx: double-entry ledger pair (debit advertiser balance account,
//     credit platform revenue) keyed by reference_type='spend' +
//     reference_id=traceID:eventType, then the advertiser_balances
//     decrement. Same shape as the topup credit (cmd/gateway/topup.go),
//     so balance stays derivable from the ledger.
//   - NATS delivers at-least-once: a replay hits the partial unique index
//     (migration 030) via ON CONFLICT DO NOTHING → applied=false and the
//     balance is NOT decremented twice.
type BalanceStore struct {
	Store *Store
}

func (s *BalanceStore) DebitSpend(ctx context.Context, accountID string, amount float64, currency, traceID, eventType string) (newBalance float64, applied bool, err error) {
	if amount <= 0 {
		return 0, false, fmt.Errorf("debit amount must be > 0, got %v", amount)
	}
	tx, err := s.Store.primary.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return 0, false, fmt.Errorf("set tenant: %w", err)
	}

	refID := traceID + ":" + eventType
	balanceAccount := "advertiser:" + accountID + ":balance"
	res, err := tx.ExecContext(ctx,
		`INSERT INTO ledger_entries (account_code, entry_type, amount, currency, reference_type, reference_id, trace_id)
		 VALUES ($1, 'debit', $2, $3, 'spend', $4, $5),
		        ('platform:revenue', 'credit', $2, $3, 'spend', $4, $5)
		 ON CONFLICT (reference_type, reference_id, account_code) WHERE reference_type = 'spend' DO NOTHING`,
		balanceAccount, amount, currency, refID, traceID)
	if err != nil {
		return 0, false, fmt.Errorf("insert spend ledger pair: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Replay — the pair (and the decrement) already happened.
		return 0, false, tx.Commit()
	}

	if err := tx.QueryRowContext(ctx,
		`INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
		 VALUES ($1::uuid, -($2::numeric), $3, now())
		 ON CONFLICT (account_id) DO UPDATE
		   SET balance = advertiser_balances.balance - $2::numeric, updated_at = now()
		 RETURNING balance::float8`,
		accountID, amount, currency).Scan(&newBalance); err != nil {
		return 0, false, fmt.Errorf("decrement balance: %w", err)
	}

	return newBalance, true, tx.Commit()
}
