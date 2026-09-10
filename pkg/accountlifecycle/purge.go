package accountlifecycle

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// AnalyticsPurger destructively removes an account's analytics rows (ClickHouse).
// Satisfied by analytics.ClickHouse.PurgeAccount.
type AnalyticsPurger interface {
	PurgeAccount(ctx context.Context, accountID string) error
}

// purgeKeepTables are account-keyed tables the purge preserves: the closure
// record itself (the tombstone that records the purge) and the audit log
// (retained for compliance — it holds actor events, not the account's PII data).
var purgeKeepTables = map[string]bool{
	"account_closure_requests": true,
	"audit_log":                true,
}

// Purger runs the 90-day destructive account purge — the last account-closure
// deferral. It scans closures that have been 'closed' for RetentionDays and wipes
// the account's data across Postgres (every account-keyed tenant table) and
// ClickHouse (analytics), then marks the closure 'purged'. The accounts row + the
// closure record survive as a tombstone. Idempotent (only 'closed' selected) and
// best-effort per account (one failure is logged + skipped, leaving it for the
// next run).
type Purger struct {
	DB        *sql.DB
	Analytics AnalyticsPurger // nil = skip the ClickHouse half (logged)
	Now       func() time.Time
	Log       *slog.Logger
}

func (p *Purger) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// RunDuePurges purges every account whose closure has been 'closed' for at least
// RetentionDays. Returns the number of accounts purged.
func (p *Purger) RunDuePurges(ctx context.Context) (int, error) {
	due, err := p.selectDue(ctx)
	if err != nil {
		return 0, err
	}
	purged := 0
	for _, d := range due {
		if err := p.purgeOne(ctx, d.closureID, d.accountID); err != nil {
			p.Log.Error("account purge failed, will retry next run", "account", d.accountID, "closure", d.closureID, "error", err)
			continue
		}
		purged++
	}
	return purged, nil
}

type duePurge struct{ closureID, accountID string }

// selectDue lists closures 'closed' past the retention window. Platform hatch —
// the closure table has RLS; a bare read as adtech_app returns 0 rows.
func (p *Purger) selectDue(ctx context.Context) ([]duePurge, error) {
	tx, err := p.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, err
	}
	cutoff := p.now().UTC().AddDate(0, 0, -RetentionDays)
	rows, err := tx.QueryContext(ctx,
		`SELECT id::text, account_id::text FROM account_closure_requests
		 WHERE status = 'closed' AND closed_at IS NOT NULL AND closed_at <= $1`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []duePurge
	for rows.Next() {
		var d duePurge
		if err := rows.Scan(&d.closureID, &d.accountID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// purgeOne wipes one account's data everywhere, then marks the closure 'purged'.
func (p *Purger) purgeOne(ctx context.Context, closureID, accountID string) error {
	pgRows, err := p.purgePostgres(ctx, accountID)
	if err != nil {
		return fmt.Errorf("purge postgres: %w", err)
	}
	if p.Analytics != nil {
		if err := p.Analytics.PurgeAccount(ctx, accountID); err != nil {
			return fmt.Errorf("purge analytics: %w", err)
		}
	} else {
		p.Log.Warn("analytics purger not wired — ClickHouse rows NOT purged", "account", accountID)
	}
	if err := p.markPurged(ctx, closureID); err != nil {
		return fmt.Errorf("mark purged: %w", err)
	}
	p.Log.Info("account purged", "account", accountID, "closure", closureID, "pg_rows", pgRows, "analytics", p.Analytics != nil)
	return nil
}

// purgePostgres deletes the account's rows from every account-keyed tenant table
// (discovered from information_schema, so new tables are covered automatically),
// preserving the tombstone tables. FK dependencies are handled by retry passes:
// a delete blocked by a child row (23503) is retried after the child's table is
// cleared. Each delete is scoped by BOTH the account tenant-GUC (RLS) and an
// explicit WHERE account_id — belt and suspenders on a destructive write.
func (p *Purger) purgePostgres(ctx context.Context, accountID string) (int64, error) {
	tables, err := p.accountKeyedTables(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	remaining := tables
	for pass := 0; pass < 12 && len(remaining) > 0; pass++ {
		var blocked []string
		for _, tbl := range remaining {
			n, err := p.deleteAccountRows(ctx, tbl, accountID)
			if err != nil {
				if isForeignKeyViolation(err) {
					blocked = append(blocked, tbl) // a child still references these; retry next pass
					continue
				}
				return total, fmt.Errorf("delete %s: %w", tbl, err)
			}
			total += n
		}
		if len(blocked) == len(remaining) {
			return total, fmt.Errorf("purge stuck (FK cycle) on: %s", strings.Join(blocked, ", "))
		}
		remaining = blocked
	}
	return total, nil
}

// accountKeyedTables returns every public table with an account_id column, minus
// the tombstone keep-list.
func (p *Purger) accountKeyedTables(ctx context.Context) ([]string, error) {
	rows, err := p.DB.QueryContext(ctx,
		`SELECT table_name FROM information_schema.columns
		 WHERE column_name = 'account_id' AND table_schema = 'public'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		if !purgeKeepTables[t] {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// deleteAccountRows deletes one table's rows for the account, scoped by the tenant
// GUC (RLS confines the delete to this account) in its own tx so an FK failure
// doesn't poison the others.
func (p *Purger) deleteAccountRows(ctx context.Context, table, accountID string) (int64, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return 0, err
	}
	// table is from information_schema (not user input); account_id is bound.
	res, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE account_id = $1", pq(table)), accountID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// markPurged flips the closure to the terminal 'purged' state under the platform
// hatch (cross-tenant write on an RLS table).
func (p *Purger) markPurged(ctx context.Context, closureID string) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE account_closure_requests SET status = 'purged', purged_at = now(), updated_at = now()
		 WHERE id = $1::uuid AND status = 'closed'`, closureID); err != nil {
		return err
	}
	return tx.Commit()
}

// pq double-quotes an identifier (defensive — the names come from the catalog).
func pq(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

// isForeignKeyViolation reports whether err is a Postgres FK violation (23503).
func isForeignKeyViolation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "23503") || strings.Contains(err.Error(), "violates foreign key"))
}
