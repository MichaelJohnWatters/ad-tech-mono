// Package postgres implements accountexport.Store. Account-facing reads/writes
// (enqueue, latest, get) run in a tenant transaction (RLS GUC = the account);
// the worker methods (claim, mark) run under the platform hatch since they act
// cross-tenant off any tenant session.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountexport"
)

// Store is the Postgres-backed account-export store.
type Store struct{ db *sql.DB }

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

const jobCols = `id::text, account_id::text, status, COALESCE(artifact_bucket,''),
       COALESCE(artifact_key,''), COALESCE(artifact_bytes,0), COALESCE(error,''),
       COALESCE(requested_by,''), created_at, started_at, finished_at, expires_at`

func scanJob(scan func(dest ...any) error) (accountexport.Job, error) {
	var j accountexport.Job
	var started, finished sql.NullTime
	if err := scan(&j.ID, &j.AccountID, &j.Status, &j.ArtifactBucket, &j.ArtifactKey,
		&j.ArtifactBytes, &j.Error, &j.RequestedBy, &j.CreatedAt, &started, &finished, &j.ExpiresAt); err != nil {
		return accountexport.Job{}, err
	}
	if started.Valid {
		j.StartedAt = &started.Time
	}
	if finished.Valid {
		j.FinishedAt = &finished.Time
	}
	return j, nil
}

func (s *Store) tenantTx(ctx context.Context, accountID string) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("set tenant: %w", err)
	}
	return tx, nil
}

func (s *Store) platformTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("platform-read: %w", err)
	}
	return tx, nil
}

// Enqueue creates a queued export, coalescing onto an existing in-flight job.
func (s *Store) Enqueue(ctx context.Context, accountID, requestedBy string, ttl time.Duration) (accountexport.Job, error) {
	if ttl <= 0 {
		ttl = accountexport.DefaultTTL
	}
	tx, err := s.tenantTx(ctx, accountID)
	if err != nil {
		return accountexport.Job{}, err
	}
	defer tx.Rollback()

	var reqBy any
	if requestedBy != "" {
		reqBy = requestedBy
	}
	expires := time.Now().Add(ttl)
	row := tx.QueryRowContext(ctx, `
INSERT INTO account_export_jobs (account_id, status, requested_by, expires_at)
VALUES ($1::uuid, 'queued', $2, $3)
ON CONFLICT (account_id) WHERE status IN ('queued','running') DO NOTHING
RETURNING `+jobCols, accountID, reqBy, expires)
	job, err := scanJob(row.Scan)
	if err == sql.ErrNoRows {
		// An in-flight job already exists — return it instead of a new one.
		row = tx.QueryRowContext(ctx, `SELECT `+jobCols+`
FROM account_export_jobs WHERE account_id = $1::uuid AND status IN ('queued','running')
ORDER BY created_at DESC LIMIT 1`, accountID)
		job, err = scanJob(row.Scan)
	}
	if err != nil {
		return accountexport.Job{}, fmt.Errorf("enqueue export: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return accountexport.Job{}, fmt.Errorf("commit enqueue: %w", err)
	}
	return job, nil
}

// LatestForAccount returns the account's most recent export job, or nil.
func (s *Store) LatestForAccount(ctx context.Context, accountID string) (*accountexport.Job, error) {
	tx, err := s.tenantTx(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT `+jobCols+`
FROM account_export_jobs WHERE account_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, accountID)
	job, err := scanJob(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest export: %w", err)
	}
	return &job, nil
}

// Get returns one job by id scoped to the account.
func (s *Store) Get(ctx context.Context, accountID, id string) (*accountexport.Job, error) {
	tx, err := s.tenantTx(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT `+jobCols+`
FROM account_export_jobs WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	job, err := scanJob(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get export: %w", err)
	}
	return &job, nil
}

// ClaimNext atomically claims the oldest queued job (worker, cross-tenant).
func (s *Store) ClaimNext(ctx context.Context) (*accountexport.Job, error) {
	tx, err := s.platformTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `
UPDATE account_export_jobs SET status = 'running', started_at = now(), attempts = attempts + 1
WHERE id = (
    SELECT id FROM account_export_jobs WHERE status = 'queued'
    ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
)
RETURNING `+jobCols)
	job, err := scanJob(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil // empty queue
	}
	if err != nil {
		return nil, fmt.Errorf("claim export: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &job, nil
}

// MarkDone records a completed artifact (worker).
func (s *Store) MarkDone(ctx context.Context, id, bucket, key string, bytes int64) error {
	return s.workerUpdate(ctx, `
UPDATE account_export_jobs
SET status = 'done', artifact_bucket = $2, artifact_key = $3, artifact_bytes = $4,
    error = NULL, finished_at = now()
WHERE id = $1::uuid`, id, bucket, key, bytes)
}

// MarkFailed records a failure (worker).
func (s *Store) MarkFailed(ctx context.Context, id, errMsg string) error {
	return s.workerUpdate(ctx, `
UPDATE account_export_jobs SET status = 'failed', error = $2, finished_at = now()
WHERE id = $1::uuid`, id, errMsg)
}

func (s *Store) workerUpdate(ctx context.Context, query string, args ...any) error {
	tx, err := s.platformTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("worker update: %w", err)
	}
	return tx.Commit()
}
