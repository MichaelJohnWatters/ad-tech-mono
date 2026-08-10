package accountexport

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// Builder materializes an account's data into a zip archive in object storage.
type Builder struct {
	DB      *sql.DB
	Objects objects.Store
	Bucket  string
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// Build gathers the account's data into a zip and uploads it. Returns the object
// key + byte size. All reads run in ONE tenant transaction (RLS GUC = the
// account) so every table is scoped to this account; audit_log (no RLS) is
// filtered explicitly.
func (b *Builder) Build(ctx context.Context, accountID, jobID string) (string, int64, error) {
	if b.Objects == nil {
		return "", 0, fmt.Errorf("account export: object store unavailable")
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}

	tx, err := b.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", 0, fmt.Errorf("set tenant: %w", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	counts := map[string]int{}

	// Each section: a CSV file inside the zip. Order is stable for reproducibility.
	sections := []struct {
		file    string
		header  []string
		query   string
		args    []any
		scanRow func(rows *sql.Rows) ([]string, error)
	}{
		{
			file:   "campaigns.csv",
			header: []string{"line_item_id", "insertion_order", "name", "status", "format", "bid_strategy", "base_bid", "daily_budget", "created_at"},
			query: `SELECT li.id::text, COALESCE(io.name,''), li.name, li.status, li.format, li.bid_strategy,
			         li.base_bid, li.daily_budget, li.created_at
			         FROM line_items li LEFT JOIN insertion_orders io ON io.id = li.insertion_order_id
			         ORDER BY li.created_at DESC`,
			scanRow: func(rows *sql.Rows) ([]string, error) {
				var id, io, name, status, format, strat string
				var baseBid, daily float64
				var created time.Time
				if err := rows.Scan(&id, &io, &name, &status, &format, &strat, &baseBid, &daily, &created); err != nil {
					return nil, err
				}
				return []string{id, io, name, status, format, strat, f2(baseBid), f2(daily), created.Format(time.RFC3339)}, nil
			},
		},
		{
			file:   "creatives.csv",
			header: []string{"creative_id", "name", "format", "width", "height", "landing_url", "review_status", "created_at"},
			query: `SELECT id::text, name, format, COALESCE(width,0), COALESCE(height,0),
			         COALESCE(landing_url,''), review_status, created_at FROM creatives ORDER BY created_at DESC`,
			scanRow: func(rows *sql.Rows) ([]string, error) {
				var id, name, format, landing, review string
				var w, h int
				var created time.Time
				if err := rows.Scan(&id, &name, &format, &w, &h, &landing, &review, &created); err != nil {
					return nil, err
				}
				return []string{id, name, format, itoa(w), itoa(h), landing, review, created.Format(time.RFC3339)}, nil
			},
		},
		{
			// Aggregate only — segment metadata + member COUNT, never member ids.
			file:   "audiences.csv",
			header: []string{"segment_id", "name", "type", "visibility", "status", "member_count"},
			query: `SELECT s.id::text, s.name, s.type, COALESCE(s.visibility,''), s.status,
			         (SELECT count(*) FROM audience_segment_members m WHERE m.segment_id = s.id AND m.account_id = $1::uuid)
			         FROM audience_segments s ORDER BY s.name`,
			args: []any{accountID},
			scanRow: func(rows *sql.Rows) ([]string, error) {
				var id, name, typ, vis, status string
				var members int64
				if err := rows.Scan(&id, &name, &typ, &vis, &status, &members); err != nil {
					return nil, err
				}
				return []string{id, name, typ, vis, status, fmt.Sprintf("%d", members)}, nil
			},
		},
		{
			file:   "invoices.csv",
			header: []string{"invoice_id", "period_start", "period_end", "total", "currency", "status", "due_date"},
			query: `SELECT id::text, period_start, period_end, total, currency, status, due_date
			         FROM invoices ORDER BY period_start DESC`,
			scanRow: func(rows *sql.Rows) ([]string, error) {
				var id, currency, status string
				var total float64
				var ps, pe, due time.Time
				if err := rows.Scan(&id, &ps, &pe, &total, &currency, &status, &due); err != nil {
					return nil, err
				}
				return []string{id, ps.Format("2006-01-02"), pe.Format("2006-01-02"), f2(total), currency, status, due.Format("2006-01-02")}, nil
			},
		},
		{
			file:   "payouts.csv",
			header: []string{"payout_id", "period_start", "period_end", "amount", "currency", "platform_fee", "status"},
			query: `SELECT id::text, period_start, period_end, amount, currency, platform_fee, status
			         FROM payouts ORDER BY period_start DESC`,
			scanRow: func(rows *sql.Rows) ([]string, error) {
				var id, currency, status string
				var amount, fee float64
				var ps, pe time.Time
				if err := rows.Scan(&id, &ps, &pe, &amount, &currency, &fee, &status); err != nil {
					return nil, err
				}
				return []string{id, ps.Format("2006-01-02"), pe.Format("2006-01-02"), f2(amount), currency, f2(fee), status}, nil
			},
		},
		{
			// audit_log has no RLS — filter explicitly by account_id.
			file:   "audit_log.csv",
			header: []string{"timestamp", "actor_id", "action", "resource_type", "resource_id", "reason"},
			query: `SELECT "timestamp", COALESCE(actor_id,''), action, COALESCE(resource_type,''),
			         COALESCE(resource_id,''), COALESCE(reason,'') FROM audit_log
			         WHERE account_id = $1::uuid ORDER BY "timestamp" DESC LIMIT 10000`,
			args: []any{accountID},
			scanRow: func(rows *sql.Rows) ([]string, error) {
				var actor, action, rtype, rid, reason string
				var ts time.Time
				if err := rows.Scan(&ts, &actor, &action, &rtype, &rid, &reason); err != nil {
					return nil, err
				}
				return []string{ts.Format(time.RFC3339), actor, action, rtype, rid, reason}, nil
			},
		},
	}

	for _, sec := range sections {
		n, err := writeCSVSection(ctx, tx, zw, sec.file, sec.header, sec.query, sec.args, sec.scanRow)
		if err != nil {
			return "", 0, fmt.Errorf("%s: %w", sec.file, err)
		}
		counts[sec.file] = n
	}

	// metadata.json: who/when + row counts, so a recipient can sanity-check.
	var acctName, acctType string
	_ = tx.QueryRowContext(ctx, `SELECT name, type FROM accounts WHERE id = $1::uuid`, accountID).Scan(&acctName, &acctType)
	meta := map[string]any{
		"account_id":   accountID,
		"account_name": acctName,
		"account_type": acctType,
		"exported_at":  now().UTC().Format(time.RFC3339),
		"row_counts":   counts,
		"note":         "Aggregate audience counts only (no member ids). Creative metadata only (no binary assets).",
	}
	mw, err := zw.Create("metadata.json")
	if err != nil {
		return "", 0, fmt.Errorf("metadata: %w", err)
	}
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(meta); err != nil {
		return "", 0, fmt.Errorf("encode metadata: %w", err)
	}

	if err := zw.Close(); err != nil {
		return "", 0, fmt.Errorf("close zip: %w", err)
	}
	_ = tx.Rollback() // read-only; release before the upload

	key := fmt.Sprintf("%s/account-export-%s.zip", accountID, jobID)
	size := int64(buf.Len())
	if err := b.Objects.Put(ctx, b.Bucket, key, &buf, size, "application/zip"); err != nil {
		return "", 0, fmt.Errorf("upload archive: %w", err)
	}
	return key, size, nil
}

// writeCSVSection runs the query and writes a CSV file into the zip. Returns the
// row count.
func writeCSVSection(ctx context.Context, tx *sql.Tx, zw *zip.Writer, file string, header []string, query string, args []any, scanRow func(*sql.Rows) ([]string, error)) (int, error) {
	fw, err := zw.Create(file)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(fw)
	if err := cw.Write(header); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return 0, err
		}
		if err := cw.Write(rec); err != nil {
			return 0, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	cw.Flush()
	return n, cw.Error()
}

func f2(v float64) string { return fmt.Sprintf("%.2f", v) }
func itoa(v int) string   { return fmt.Sprintf("%d", v) }
