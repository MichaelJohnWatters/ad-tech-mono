package accountlifecycle

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// RetentionDays is how long a closed account's data is retained before the
// (deferred) purge runs — PLAN: 90 days.
const RetentionDays = 90

// InvoiceGenerator is the settlement seam the close-out needs. Satisfied by
// invoicing.Generator (GenerateForAccount writes nothing when there's no
// un-billed spend, returning an empty id + nil error).
type InvoiceGenerator interface {
	GenerateForAccount(ctx context.Context, accountID string, periodStart, periodEnd time.Time) (string, error)
}

// CloseOut finalizes account closures whose grace period has elapsed: it
// generates a final invoice (advertisers), marks the account closed, and closes
// the request. The actual 90-day data purge is DEFERRED (a documented follow-up)
// — this schedules it by leaving closed_at set, which the future purge job will
// scan (closed_at + RetentionDays <= now).
type CloseOut struct {
	DB       *sql.DB
	Invoices InvoiceGenerator
	Now      func() time.Time
	Log      *slog.Logger
}

func (c *CloseOut) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// dueClosure is a grace closure past its window.
type dueClosure struct {
	id, accountID string
	graceEndsAt   time.Time
}

// RunDue processes every grace closure whose window has elapsed. It is
// idempotent (each closure flips grace→closed exactly once) and best-effort
// per account: one account's failure is logged and skipped, not fatal. Returns
// the number of accounts closed out.
func (c *CloseOut) RunDue(ctx context.Context) (int, error) {
	due, err := c.selectDue(ctx)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, d := range due {
		if err := c.closeOne(ctx, d); err != nil {
			c.Log.Error("account close-out failed", "account", d.accountID, "closure", d.id, "error", err)
			continue
		}
		closed++
	}
	return closed, nil
}

func (c *CloseOut) selectDue(ctx context.Context) ([]dueClosure, error) {
	tx, err := c.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin select-due: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("select-due platform-read: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id::text, account_id::text, grace_ends_at
FROM account_closure_requests
WHERE status = 'grace' AND grace_ends_at <= $1
ORDER BY grace_ends_at`, c.now())
	if err != nil {
		return nil, fmt.Errorf("select due closures: %w", err)
	}
	defer rows.Close()
	var out []dueClosure
	for rows.Next() {
		var d dueClosure
		if err := rows.Scan(&d.id, &d.accountID, &d.graceEndsAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (c *CloseOut) closeOne(ctx context.Context, d dueClosure) error {
	// Account type decides the settlement path.
	var acctType string
	var createdAt time.Time
	if err := c.DB.QueryRowContext(ctx,
		`SELECT type, created_at FROM accounts WHERE id = $1::uuid`, d.accountID).Scan(&acctType, &createdAt); err != nil {
		return fmt.Errorf("load account: %w", err)
	}

	// Final settlement.
	switch acctType {
	case "advertiser", "agency":
		if c.Invoices != nil {
			// Bill the un-billed tail on DAY boundaries: committed spend is keyed by
			// DATE, so a timestamp window would drop today's spend. End = start of
			// tomorrow so today is included.
			start := c.finalInvoiceStart(ctx, d.accountID, createdAt)
			end := dayFloor(c.now().UTC()).AddDate(0, 0, 1)
			id, err := c.Invoices.GenerateForAccount(ctx, d.accountID, start, end)
			if err != nil {
				return fmt.Errorf("final invoice: %w", err)
			}
			if id != "" {
				c.Log.Info("final invoice generated", "account", d.accountID, "invoice", id)
			}
		}
	case "publisher":
		// Publisher final payout reuses the existing payout path — deferred (no
		// payout generator exists yet). Documented follow-up.
		c.Log.Info("publisher final payout deferred", "account", d.accountID)
	}

	// Flip account + closure to closed under the platform hatch (cross-tenant).
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin close: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return fmt.Errorf("close platform-read: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE accounts SET status = 'closed', updated_at = now() WHERE id = $1::uuid`, d.accountID); err != nil {
		return fmt.Errorf("mark account closed: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE account_closure_requests SET status = 'closed', closed_at = now(), updated_at = now()
		 WHERE id = $1::uuid AND status = 'grace'`, d.id); err != nil {
		return fmt.Errorf("mark closure closed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit close: %w", err)
	}

	purgeDue := c.now().AddDate(0, 0, RetentionDays)
	c.Log.Info("account closed out", "account", d.accountID, "closure", d.id,
		"purge_scheduled", purgeDue.Format(time.RFC3339), "note", "purge deferred (documented follow-up)")
	return nil
}

// finalInvoiceStart is the start of the un-billed tail: the latest existing
// invoice's period_end, or the account's creation date when none exists. Keeps
// the final invoice from overlapping monthly ones.
func (c *CloseOut) finalInvoiceStart(ctx context.Context, accountID string, createdAt time.Time) time.Time {
	tx, err := c.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return createdAt
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return createdAt
	}
	var last sql.NullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT max(period_end) FROM invoices WHERE account_id = $1::uuid`, accountID).Scan(&last); err != nil {
		return createdAt
	}
	if last.Valid && last.Time.After(createdAt) {
		return last.Time
	}
	return dayFloor(createdAt.UTC())
}

// dayFloor truncates to the start of the UTC day (committed spend is DATE-keyed).
func dayFloor(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
