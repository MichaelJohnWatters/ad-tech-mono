// Package postgres is the Postgres store for the ARA reporting-only overlay
// (pkg/ara). Writes are tenant-scoped to the advertiser account; the report
// ingest — an UNAUTHENTICATED, cross-tenant browser POST — resolves the owning
// account through the RLS platform-read hatch before scoping the report write.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

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

// ResolveAccount finds the advertiser account for an incoming report. The report
// ingest is UNAUTHENTICATED (browsers POST it), so resolution is the only thing
// standing between a report and a tenant's rows — it must not be forgeable.
//
// The ONLY key we trust is the source_event_id we minted (a 64-bit random the
// attacker can't guess). We deliberately do NOT fall back to the report's
// attribution_destination: that is the advertiser's PUBLIC site, nameable by
// anyone, so resolving on it would let an unauthenticated POST write into another
// tenant's rows (see docs/ara-review-findings.md, F1). Aggregatable reports carry
// no source_event_id and so never resolve here — the caller quarantines them
// instead (QuarantineAggregatable). A report that resolves to nothing is dropped
// by the caller. Reads cross-tenant via the platform hatch (the POST has no
// tenant context).
func (s *Store) ResolveAccount(ctx context.Context, sourceEventID string) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	if sourceEventID == "" {
		return "", ErrNoAccount
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
	err = tx.QueryRowContext(ctx,
		`SELECT account_id::text FROM ara_sources WHERE source_event_id = $1`, sourceEventID).Scan(&acct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoAccount
	}
	if err != nil {
		return "", err
	}
	return acct, nil
}

// QuarantineAggregatable persists an aggregatable report to the platform-global
// quarantine (migration 096) — NOT tied to any tenant, never shown in an
// advertiser overlay. Aggregatable reports carry no unguessable source id, so they
// can only be matched to an advertiser by the public destination; attributing on
// that basis is a cross-tenant write (F1), and we can't verify the payloads
// without the aggregation service (the mock boundary) anyway. So we hold them
// here for staff inspection. Idempotent on the browser's report_id. Returns
// (inserted, error). The quarantine table has no RLS, so no GUC is needed.
func (s *Store) QuarantineAggregatable(ctx context.Context, claimedDest, reportID string, body []byte) (bool, error) {
	if s.db == nil {
		return false, sql.ErrConnDone
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO ara_aggregatable_quarantine (claimed_destination, report_id, body)
VALUES ($1, $2, $3::jsonb)
ON CONFLICT DO NOTHING`, claimedDest, nullStr(reportID), string(body))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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

// purgeBatch bounds each housekeeping DELETE so a backlog (purge loop down for a
// long window, or a registration flood) can't turn into one giant long-locking
// transaction (F3). DeleteExpiredSources / DeleteOldQuarantine loop in this many
// rows at a time until drained.
const purgeBatch = 5000

// DeleteExpiredSources removes source-registration rows past their expiry — pure
// housekeeping (expired sources already don't resolve). Platform-wide, so it uses
// the hatch. Batched: a single unbounded DELETE could lock the table for a long
// time under a backlog, so we delete in bounded chunks until drained.
func (s *Store) DeleteExpiredSources(ctx context.Context) (int64, error) {
	if s.db == nil {
		return 0, sql.ErrConnDone
	}
	var total int64
	for {
		n, err := s.deleteBatch(ctx,
			`DELETE FROM ara_sources WHERE ctid IN (
			   SELECT ctid FROM ara_sources WHERE expires_at < now() LIMIT $1)`)
		if err != nil {
			return total, err
		}
		total += n
		if n < purgeBatch {
			return total, nil
		}
	}
}

// DeleteOldQuarantine drops aggregatable-quarantine rows older than maxAge. The
// quarantine is fed by an unauthenticated ingest, so it must be age-bounded.
// Platform-global table (no RLS) — no hatch needed. Batched like the above.
func (s *Store) DeleteOldQuarantine(ctx context.Context, maxAge time.Duration) (int64, error) {
	if s.db == nil {
		return 0, sql.ErrConnDone
	}
	cutoff := time.Now().Add(-maxAge)
	var total int64
	for {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM ara_aggregatable_quarantine WHERE ctid IN (
			   SELECT ctid FROM ara_aggregatable_quarantine WHERE received_at < $1 LIMIT $2)`,
			cutoff, purgeBatch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < purgeBatch {
			return total, nil
		}
	}
}

// deleteBatch runs one bounded platform-hatch DELETE and returns the row count.
func (s *Store) deleteBatch(ctx context.Context, query string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, query, purgeBatch)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
