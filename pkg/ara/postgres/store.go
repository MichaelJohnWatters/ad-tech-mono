// Package postgres is the Postgres store for the ARA reporting-only overlay
// (pkg/ara). Writes are tenant-scoped to the advertiser account; the report
// ingest — an UNAUTHENTICATED, cross-tenant browser POST — resolves the owning
// account through the RLS platform-read hatch before scoping the report write.
package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara"
)

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

// RecordSource logs a registered source under its advertiser account so a later
// report can be resolved back to that account. Idempotent on source_event_id.
func (s *Store) RecordSource(ctx context.Context, reg ara.SourceRegistration) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, reg.AccountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO ara_sources (source_event_id, account_id, destination, campaign_id, expires_at)
VALUES ($1, $2::uuid, $3, $4, $5)
ON CONFLICT (source_event_id) DO NOTHING`,
		reg.SourceEventID, reg.AccountID, reg.Destination, nullStr(reg.CampaignID), reg.ExpiresAt); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrNoAccount means a report couldn't be tied to any registered source — it is
// dropped (accepted, not persisted) so the browser doesn't retry forever.
var ErrNoAccount = errors.New("ara: no account for report")

// ResolveAccount finds the advertiser account for an incoming report. Event
// reports carry the source_event_id we minted (exact match); aggregatable
// reports don't, so they fall back to the newest unexpired source for the
// destination. Reads cross-tenant via the platform hatch (the browser POST has
// no tenant context).
func (s *Store) ResolveAccount(ctx context.Context, sourceEventID, destination string) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return "", err
	}
	var acct string
	if sourceEventID != "" {
		err = tx.QueryRowContext(ctx,
			`SELECT account_id::text FROM ara_sources WHERE source_event_id = $1`, sourceEventID).Scan(&acct)
		if err == nil {
			return acct, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	// Fall back to the destination (aggregatable reports, or an unknown source).
	err = tx.QueryRowContext(ctx,
		`SELECT account_id::text FROM ara_sources
		  WHERE destination = $1 AND expires_at > now()
		  ORDER BY registered_at DESC LIMIT 1`, destination).Scan(&acct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoAccount
	}
	if err != nil {
		return "", err
	}
	return acct, nil
}

// SaveReport persists a report under its resolved account. Idempotent on the
// browser's report_id (the partial unique index) so a retried delivery is a
// no-op. Returns (inserted, error).
func (s *Store) SaveReport(ctx context.Context, accountID string, r ara.StoredReport) (bool, error) {
	if s.db == nil {
		return false, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO ara_reports (account_id, report_type, attribution_destination, source_event_id, trigger_data, report_id, body)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb)
ON CONFLICT DO NOTHING`,
		accountID, string(r.ReportType), r.AttributionDestination,
		nullStr(r.SourceEventID), nullStr(r.TriggerData), nullStr(r.ReportID), string(r.Body))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListReports returns an account's most recent ARA reports (tenant-scoped).
func (s *Store) ListReports(ctx context.Context, accountID string, limit int) ([]ara.StoredReport, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id::text, report_type, attribution_destination,
       COALESCE(source_event_id,''), COALESCE(trigger_data,''), COALESCE(report_id,''),
       body, received_at
FROM ara_reports ORDER BY received_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ara.StoredReport{}
	for rows.Next() {
		var r ara.StoredReport
		var body []byte
		var rt string
		if err := rows.Scan(&r.ID, &rt, &r.AttributionDestination,
			&r.SourceEventID, &r.TriggerData, &r.ReportID, &body, &r.ReceivedAt); err != nil {
			return nil, err
		}
		r.ReportType = ara.ReportType(rt)
		r.Body = body
		out = append(out, r)
	}
	return out, rows.Err()
}

// Summary is a per-type count for the reporting overlay surface.
type Summary struct {
	Event     int `json:"event"`
	Aggregate int `json:"aggregate"`
}

// SummaryForAccount returns the account's ARA report counts by type.
func (s *Store) SummaryForAccount(ctx context.Context, accountID string) (Summary, error) {
	var sum Summary
	if s.db == nil {
		return sum, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return sum, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return sum, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT report_type, count(*) FROM ara_reports GROUP BY report_type`)
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var rt string
		var n int
		if err := rows.Scan(&rt, &n); err != nil {
			return sum, err
		}
		switch ara.ReportType(rt) {
		case ara.ReportEvent:
			sum.Event = n
		case ara.ReportAggregate:
			sum.Aggregate = n
		}
	}
	return sum, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
