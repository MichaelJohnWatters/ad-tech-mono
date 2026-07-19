package postgres

import (
	"context"
	"fmt"

	"github.com/lib/pq"
)

// AdvertiserBalance is one advertiser account's prepay balance as the DSP's
// warm cache stores it. The bid path gates on Balance (+ CreditLimit for
// invoiced accounts) minus the Redis spend mirror — see cmd/dsp BalanceGate.
type AdvertiserBalance struct {
	AccountID string
	Balance   float64
	Currency  string
	// PaymentTerms is 'prepay' (default) or 'invoiced'. CreditLimit is the
	// invoiced headroom in DECIMAL dollars: an account may bid while
	// balance + credit_limit − spend > 0. Prepay rows carry credit_limit 0,
	// so the gate formula reduces to the prepay behaviour exactly.
	PaymentTerms string
	CreditLimit  float64
}

// BalanceLoader reads every advertiser balance row. One cheap SELECT — the
// table has one row per advertiser account (topup upserts it).
type BalanceLoader struct {
	Store *Store
}

func (l *BalanceLoader) LoadAll(ctx context.Context) ([]AdvertiserBalance, error) {
	const q = `
SELECT account_id::text, balance::float8, currency, payment_terms,
       COALESCE(credit_limit, 0)::float8
FROM advertiser_balances`

	rows, err := l.Store.read.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query advertiser balances: %w", err)
	}
	defer rows.Close()

	var out []AdvertiserBalance
	for rows.Next() {
		var b AdvertiserBalance
		if err := rows.Scan(&b.AccountID, &b.Balance, &b.Currency, &b.PaymentTerms, &b.CreditLimit); err != nil {
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

// BatchDebit is one realized spend to apply against an advertiser's balance.
type BatchDebit struct {
	AccountID string
	Amount    float64
	Currency  string
	TraceID   string
	EventType string
}

// BatchDebitResult is the post-decrement balance for an advertiser that had at
// least one NEW debit applied in the batch (replays that inserted nothing don't
// appear).
type BatchDebitResult struct {
	AccountID  string
	NewBalance float64
}

// DebitSpendBatch applies many realized spends in ONE round-trip (vs one tx per
// event). It is the batched twin of DebitSpend with identical semantics:
//
//   - Inserts every (debit advertiser-balance, credit platform-revenue) ledger
//     pair in a single statement, keyed idempotently by
//     reference_type='spend' + reference_id=traceID:eventType + account_code
//     (ON CONFLICT DO NOTHING, the migration-030 partial unique index). A
//     replayed event's pair is skipped.
//   - Decrements each advertiser's balance by the sum of ONLY the newly-inserted
//     debit rows (the INSERT ... RETURNING joined back to the input) — so a
//     partially-replayed batch never double-debits, exactly as DebitSpend's
//     RowsAffected==0 guard does per event.
//
// advertiser_balances and ledger_entries carry no RLS (only tenant-facing
// tables do), so a batch spanning advertisers needs no per-tenant set_config.
// In-batch duplicate (traceID,eventType) can't occur: the consumer dedups on
// NATS stream sequence before billing.
func (s *BalanceStore) DebitSpendBatch(ctx context.Context, debits []BatchDebit) ([]BatchDebitResult, error) {
	if len(debits) == 0 {
		return nil, nil
	}
	accountIDs := make([]string, len(debits))
	balAccounts := make([]string, len(debits))
	amounts := make([]float64, len(debits))
	currencies := make([]string, len(debits))
	refIDs := make([]string, len(debits))
	traceIDs := make([]string, len(debits))
	for i, d := range debits {
		accountIDs[i] = d.AccountID
		balAccounts[i] = "advertiser:" + d.AccountID + ":balance"
		amounts[i] = d.Amount
		currencies[i] = d.Currency
		refIDs[i] = d.TraceID + ":" + d.EventType
		traceIDs[i] = d.TraceID
	}

	tx, err := s.Store.primary.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	const q = `
WITH input AS (
  SELECT * FROM unnest($1::uuid[], $2::text[], $3::numeric[], $4::text[], $5::text[], $6::text[])
    AS t(account_id, bal_account, amount, currency, ref_id, trace_id)
),
ins AS (
  INSERT INTO ledger_entries (account_code, entry_type, amount, currency, reference_type, reference_id, trace_id)
  SELECT bal_account, 'debit', amount, currency, 'spend', ref_id, trace_id FROM input
  UNION ALL
  SELECT 'platform:revenue', 'credit', amount, currency, 'spend', ref_id, trace_id FROM input
  ON CONFLICT (reference_type, reference_id, account_code) WHERE reference_type = 'spend' DO NOTHING
  RETURNING account_code, reference_id, amount
),
applied AS (
  SELECT i.account_id, i.currency, sum(ins.amount) AS delta
  FROM ins
  JOIN input i ON ins.reference_id = i.ref_id AND ins.account_code = i.bal_account
  GROUP BY i.account_id, i.currency
),
upd AS (
  INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
  SELECT account_id, -delta, currency, now() FROM applied
  ON CONFLICT (account_id) DO UPDATE
    SET balance = advertiser_balances.balance + EXCLUDED.balance, updated_at = now()
  RETURNING account_id::text, balance::float8
)
SELECT account_id, balance FROM upd`

	rows, err := tx.QueryContext(ctx, q,
		pq.Array(accountIDs), pq.Array(balAccounts), pq.Array(amounts),
		pq.Array(currencies), pq.Array(refIDs), pq.Array(traceIDs))
	if err != nil {
		return nil, fmt.Errorf("batch debit: %w", err)
	}
	var results []BatchDebitResult
	for rows.Next() {
		var r BatchDebitResult
		if err := rows.Scan(&r.AccountID, &r.NewBalance); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan batch debit: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return results, tx.Commit()
}
