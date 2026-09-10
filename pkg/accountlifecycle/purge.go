package accountlifecycle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// AnalyticsPurger destructively removes an account's analytics rows (ClickHouse).
// Satisfied by analytics.ClickHouse.PurgeAccount.
type AnalyticsPurger interface {
	PurgeAccount(ctx context.Context, accountID string) error
}

// purgeableTables is an EXPLICIT ALLOWLIST of the account's private, operational
// data that the 90-day purge destroys. A destructive op must FAIL SAFE: a new
// account-keyed table is NOT swept in until someone deliberately adds it here
// (an information_schema denylist would auto-destroy new tables — including
// financial or shared ones — silently). Deliberately EXCLUDED and RETAINED:
//   - financial/legal (tax retention): invoices, invoice_line_items, payouts,
//     adjustments, topups, advertiser_balances, data_fee_earnings.
//   - tombstone/audit: account_closure_requests, audit_log.
//   - platform-global (not the account's data): partners.
//   - cross-tenant-referenced (deleting harms OTHER tenants): marketplace_listings
//     (buyers' marketplace_grants cascade off it), data_providers (other tenants'
//     segments reference it).
//
// Object-storage blobs (purgeObjects, before the PG rows): the export ZIP behind
// account_export_jobs AND the account's transcoded SSAI creative segments (keyed by
// the account's own creative ids) ARE deleted. Shared/demo creative theme objects
// (creatives.asset_url → themes/…) are NOT — reused across accounts; there is no
// advertiser creative-upload path writing account-scoped blobs yet.
var purgeableTables = map[string]bool{
	// Campaign / serving config
	"line_items": true, "publisher_line_items": true, "insertion_orders": true,
	"creatives": true, "placements": true, "targeting_rules": true, "deals": true,
	"conversion_configs": true, "creative_review_queue": true, "quality_controls": true,
	// Audience / identity / retargeting (PII-adjacent)
	"audience_segments": true, "audience_segment_members": true, "audience_mappings": true,
	"audience_ingest_jobs": true, "audience_membership_changelog": true,
	"retargeting_product_views": true, "retargeting_suppressions": true, "products": true,
	// Attribution / Privacy Sandbox
	"ara_reports": true, "ara_sources": true,
	// Account operational + auth (PII / credentials)
	"notifications": true, "notification_preferences": true, "saved_reports": true,
	"report_jobs": true, "support_tickets": true, "webhooks": true, "team_members": true,
	"api_keys": true, "secrets": true, "payout_methods": true, "account_export_jobs": true,
}

// Purger runs the 90-day destructive account purge — the last account-closure
// deferral. It scans closures that have been 'closed' for RetentionDays and wipes
// the account's private data across Postgres (the purgeableTables allowlist) and
// ClickHouse (analytics), then marks the closure 'purged'. The accounts row, the
// closure record, and the retained tables (financial/legal records, platform-global
// and cross-tenant-referenced tables — see purgeableTables) survive. Idempotent
// (only 'closed' selected) and best-effort per account (one failure is logged +
// skipped, leaving it for the next run).
type Purger struct {
	DB        *sql.DB
	Analytics AnalyticsPurger // nil = skip the ClickHouse half (logged)
	Objects   objects.Store   // nil = skip object-storage blobs (logged)
	// CreativesBucket + CondPrefix locate the account's transcoded SSAI segments
	// (keyed {CondPrefix}/{creativeID}/…). Empty = skip that class. Set from
	// s3.bucket + transcoder.prefix config.
	CreativesBucket string
	CondPrefix      string
	Now             func() time.Time
	Log             *slog.Logger
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
// Order matters: object-storage blobs are deleted FIRST, while their Postgres
// pointer rows still exist to locate them; a failure here aborts before markPurged
// so the account is retried (never marked 'purged' with the full-data-copy export
// zip still sitting in the bucket).
func (p *Purger) purgeOne(ctx context.Context, closureID, accountID string) error {
	if err := p.purgeObjects(ctx, accountID); err != nil {
		return fmt.Errorf("purge objects: %w", err)
	}
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

// purgePostgres deletes the account's rows from every purgeable tenant table (the
// allowlist ∩ tables that actually carry an account_id column). FK dependencies
// are handled by retry passes:
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

// purgeObjects deletes the account's object-storage blobs whose Postgres pointer
// rows are about to be purged:
//   - the export ZIP (account_export_jobs) — a full copy of the account's exported
//     data in the private reports bucket;
//   - the account's transcoded SSAI creative segments ({CondPrefix}/{creativeID}/…
//     in the creatives bucket) — the one class of account-OWNED creative blob that
//     exists today, keyed by the account's own (UUID) creative ids so deletion is
//     tenant-safe.
//
// Any failure aborts the purge (so the account is retried, never marked 'purged'
// with a blob still in a bucket). Shared/demo creative theme assets
// (creatives.asset_url → themes/…) are NOT purged: they are reused across accounts,
// and there is no advertiser creative-upload path that writes account-scoped blobs
// (a follow-up if that feature ever lands).
func (p *Purger) purgeObjects(ctx context.Context, accountID string) error {
	if p.Objects == nil {
		p.Log.Warn("object store not wired — export artifacts + creative segments NOT purged", "account", accountID)
		return nil
	}
	// Export ZIPs.
	arts, err := p.exportArtifacts(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list export artifacts: %w", err)
	}
	for _, a := range arts {
		exists, err := p.Objects.Exists(ctx, a.bucket, a.key)
		if err != nil {
			return fmt.Errorf("stat %s/%s: %w", a.bucket, a.key, err)
		}
		if !exists {
			continue // already gone (idempotent re-run, or never materialised)
		}
		if err := p.Objects.Delete(ctx, a.bucket, a.key); err != nil {
			return fmt.Errorf("delete %s/%s: %w", a.bucket, a.key, err)
		}
		p.Log.Info("purged export artifact", "account", accountID, "bucket", a.bucket, "key", a.key)
	}
	// Transcoded SSAI creative segments — per the account's own creative ids.
	if p.CreativesBucket != "" && p.CondPrefix != "" {
		creativeIDs, err := p.accountCreativeIDs(ctx, accountID)
		if err != nil {
			return fmt.Errorf("list creative ids: %w", err)
		}
		for _, cid := range creativeIDs {
			// UUID ids are fixed-length so this prefix bounds exactly one creative's
			// objects — both {cid}/… and {cid}-{version}/… variants.
			prefix := strings.TrimRight(p.CondPrefix, "/") + "/" + cid
			keys, err := p.Objects.List(ctx, p.CreativesBucket, prefix)
			if err != nil {
				return fmt.Errorf("list segments %s/%s: %w", p.CreativesBucket, prefix, err)
			}
			for _, k := range keys {
				if err := p.Objects.Delete(ctx, p.CreativesBucket, k); err != nil {
					return fmt.Errorf("delete segment %s/%s: %w", p.CreativesBucket, k, err)
				}
			}
			if len(keys) > 0 {
				p.Log.Info("purged transcoded creative segments", "account", accountID, "creative", cid, "objects", len(keys))
			}
		}
	}
	return nil
}

// accountCreativeIDs lists the account's creative ids. Platform hatch — creatives
// has RLS; a bare read as adtech_app returns 0 rows. Read BEFORE the Postgres purge
// deletes the creatives rows.
func (p *Purger) accountCreativeIDs(ctx context.Context, accountID string) ([]string, error) {
	tx, err := p.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text FROM creatives WHERE account_id = $1::uuid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

type objectRef struct{ bucket, key string }

// exportArtifacts lists the account's export-zip object locations. Platform hatch —
// account_export_jobs has RLS; a bare read as adtech_app returns 0 rows.
func (p *Purger) exportArtifacts(ctx context.Context, accountID string) ([]objectRef, error) {
	tx, err := p.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT artifact_bucket, artifact_key FROM account_export_jobs
		 WHERE account_id = $1::uuid AND coalesce(artifact_bucket, '') <> '' AND coalesce(artifact_key, '') <> ''`,
		accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []objectRef
	for rows.Next() {
		var r objectRef
		if err := rows.Scan(&r.bucket, &r.key); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// accountKeyedTables returns the purge allowlist intersected with the tables that
// actually carry an account_id column (guards against a rename/drop leaving a
// stale allowlist entry, and skips any allowlisted table missing the column).
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
		if purgeableTables[t] {
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
	// table is a catalog identifier (not user input); account_id is bound.
	res, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE account_id = $1", pq.QuoteIdentifier(table)), accountID)
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

// isForeignKeyViolation reports whether err is a Postgres FK violation (SQLSTATE
// 23503) — checked via the typed driver code, not fragile message-string matching.
func isForeignKeyViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23503"
}
