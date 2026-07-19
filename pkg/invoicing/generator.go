// Package invoicing turns billed spend into advertiser invoices.
//
// The authoritative spend source is campaign_committed_spend (per campaign per
// day, MICRO-dollars settled). The Generator sums settled_micros per campaign
// over an invoice period, joins campaign_id → line_items to attribute each
// campaign to its advertiser account (campaign_id == line_items.id everywhere
// in this codebase), converts MICROS → dollars, and writes ONE invoices row
// plus one invoice_line_items row per campaign.
//
// Idempotency: generation is keyed on the invoices UNIQUE (account_id,
// period_start, period_end) constraint (migration 051). Re-running the same
// period UPDATEs the header total and REPLACES the line items rather than
// inserting a duplicate invoice — so the monthly CronJob (or a manual re-run)
// is safe to fire more than once.
package invoicing

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// microsPerDollar is the platform-wide money scale: 1 USD = 1,000,000 micros.
// Every settled amount in campaign_committed_spend is in micros; invoices.total
// and invoice_line_items.spend are DECIMAL dollars.
const microsPerDollar = 1_000_000.0

// dueDays is how long after the period end an invoice is due (net-30).
const dueDays = 30

// DB is the minimal database surface the Generator needs — a *sql.DB satisfies
// it, and it keeps the generator unit-testable against a real Postgres (the
// tenant GUC + RLS mean fakes can't stand in for the SQL, so tests use a real
// db via testcontainers).
type DB interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Generator writes invoices from committed spend.
type Generator struct {
	db DB
}

// New builds a Generator over the given database.
func New(db DB) *Generator { return &Generator{db: db} }

// line is one campaign's settled spend within a period.
type line struct {
	campaignID   string
	campaignName string
	bidModel     string
	spendMicros  int64
}

// GenerateForAccount writes (or refreshes) the single invoice for one account
// over [periodStart, periodEnd). It returns the invoice id. When the account
// had no settled spend in the period, no invoice is written and invoiceID is
// empty (nil error) — an empty invoice is noise, not a bill.
//
// The period is treated as [periodStart, periodEnd): committed-spend days >=
// periodStart and < periodEnd are summed, so a caller passing the first day of
// two consecutive months gets exactly the intervening month.
func (g *Generator) GenerateForAccount(ctx context.Context, accountID string, periodStart, periodEnd time.Time) (string, error) {
	lines, err := g.linesForAccount(ctx, accountID, periodStart, periodEnd)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", nil
	}

	var totalMicros int64
	for _, l := range lines {
		totalMicros += l.spendMicros
	}
	total := float64(totalMicros) / microsPerDollar
	due := periodEnd.AddDate(0, 0, dueDays)

	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin invoice tx: %w", err)
	}
	defer tx.Rollback()

	// RLS tenant GUC so the invoice + line-item writes are admitted and scoped.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", fmt.Errorf("set tenant: %w", err)
	}

	// One invoice per (account, period). On re-run, refresh the header total and
	// re-stamp the period end so the line replacement below stays consistent.
	var invoiceID string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO invoices (account_id, total, currency, status, period_start, period_end, due_date)
		 VALUES ($1::uuid, $2, 'USD', 'pending', $3, $4, $5)
		 ON CONFLICT (account_id, period_start, period_end) DO UPDATE
		   SET total = EXCLUDED.total, due_date = EXCLUDED.due_date
		 RETURNING id::text`,
		accountID, total, periodStart, periodEnd, due).Scan(&invoiceID); err != nil {
		return "", fmt.Errorf("upsert invoice: %w", err)
	}

	// Replace line items wholesale so a re-run reflects the latest committed
	// spend (a campaign's settled total can grow between runs) without leaving
	// stale rows from a prior generation.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM invoice_line_items WHERE invoice_id = $1::uuid`, invoiceID); err != nil {
		return "", fmt.Errorf("clear line items: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO invoice_line_items (invoice_id, account_id, campaign_id, campaign_name, spend, bid_model)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)`)
	if err != nil {
		return "", fmt.Errorf("prepare line insert: %w", err)
	}
	defer stmt.Close()
	for _, l := range lines {
		// impressions/clicks/conversions default to 0: committed spend carries
		// no delivery counts and there is no per-campaign-per-day count table in
		// Postgres to join cheaply. Spend is the billed source of truth.
		spend := float64(l.spendMicros) / microsPerDollar
		if _, err := stmt.ExecContext(ctx, invoiceID, accountID, l.campaignID, nullStr(l.campaignName), spend, nullStr(l.bidModel)); err != nil {
			return "", fmt.Errorf("insert line %s: %w", l.campaignID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit invoice: %w", err)
	}
	return invoiceID, nil
}

// GenerateForAllAccounts generates an invoice for every INVOICED account that
// had settled spend in the period. Prepay accounts already paid up front via
// topup, so the monthly run skips them (accountsWithSpend filters to
// payment_terms='invoiced'). It returns the invoice ids written (accounts with
// no spend, or prepay accounts, are skipped). A single account's failure aborts
// and is returned so the job surfaces it rather than silently under-billing.
//
// Staff can still invoice a specific account regardless of terms via
// GenerateForAccount (a deliberate override) — only this all-accounts monthly
// run is limited to invoiced accounts.
func (g *Generator) GenerateForAllAccounts(ctx context.Context, periodStart, periodEnd time.Time) ([]string, error) {
	accounts, err := g.accountsWithSpend(ctx, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(accounts))
	for _, acct := range accounts {
		id, err := g.GenerateForAccount(ctx, acct, periodStart, periodEnd)
		if err != nil {
			return ids, fmt.Errorf("account %s: %w", acct, err)
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// linesForAccount sums settled micros per campaign for one account over the
// period, joining committed spend → line_items for the account filter + name.
// Runs outside a tenant GUC (cross-table read like the DSP campaign loader);
// the account filter is explicit in the WHERE.
func (g *Generator) linesForAccount(ctx context.Context, accountID string, periodStart, periodEnd time.Time) ([]line, error) {
	rows, err := g.db.QueryContext(ctx,
		`SELECT ccs.campaign_id,
		        COALESCE(li.name, ''),
		        COALESCE(li.bid_strategy, ''),
		        SUM(ccs.settled_micros)::bigint
		 FROM campaign_committed_spend ccs
		 JOIN line_items li ON li.id::text = ccs.campaign_id
		 WHERE li.account_id = $1::uuid
		   AND ccs.day >= $2 AND ccs.day < $3
		 GROUP BY ccs.campaign_id, li.name, li.bid_strategy
		 HAVING SUM(ccs.settled_micros) > 0
		 ORDER BY ccs.campaign_id`,
		accountID, periodStart, periodEnd)
	if err != nil {
		return nil, fmt.Errorf("query committed spend: %w", err)
	}
	defer rows.Close()
	var out []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.campaignID, &l.campaignName, &l.bidModel, &l.spendMicros); err != nil {
			return nil, fmt.Errorf("scan committed spend: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// accountsWithSpend returns every INVOICED advertiser account with positive
// settled spend in the period (via the campaign → line_items → account_id
// join). The join to advertiser_balances with payment_terms='invoiced' excludes
// prepay accounts — they already paid up front via topup, so the monthly run
// must not bill them again. An account with spend but no advertiser_balances
// row (never topped up, default prepay) is likewise excluded.
func (g *Generator) accountsWithSpend(ctx context.Context, periodStart, periodEnd time.Time) ([]string, error) {
	rows, err := g.db.QueryContext(ctx,
		`SELECT DISTINCT li.account_id::text
		 FROM campaign_committed_spend ccs
		 JOIN line_items li ON li.id::text = ccs.campaign_id
		 JOIN advertiser_balances ab ON ab.account_id = li.account_id
		                            AND ab.payment_terms = 'invoiced'
		 WHERE ccs.day >= $1 AND ccs.day < $2 AND ccs.settled_micros > 0`,
		periodStart, periodEnd)
	if err != nil {
		return nil, fmt.Errorf("query accounts with spend: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var acct string
		if err := rows.Scan(&acct); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out = append(out, acct)
	}
	return out, rows.Err()
}

// nullStr maps "" → SQL NULL so optional text columns stay NULL rather than
// storing an empty string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LastCalendarMonth returns the [start, end) bounds of the calendar month
// preceding the reference time (both at UTC midnight, day 1). The runner
// defaults to this so the 1st-of-month CronJob bills the month that just closed.
func LastCalendarMonth(ref time.Time) (start, end time.Time) {
	ref = ref.UTC()
	firstOfThisMonth := time.Date(ref.Year(), ref.Month(), 1, 0, 0, 0, 0, time.UTC)
	start = firstOfThisMonth.AddDate(0, -1, 0)
	end = firstOfThisMonth
	return start, end
}
