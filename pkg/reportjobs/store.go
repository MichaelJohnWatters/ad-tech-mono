package reportjobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PostgresJobStore is the report_jobs queue. Tenant-facing writes set the RLS
// context; worker-facing reads span all tenants (a platform job, like the
// scheduled-report loader). The dev Postgres role bypasses RLS; a prod
// deployment needs a service role permitted to read every report_jobs row.
type PostgresJobStore struct {
	DB *sql.DB
	// WorkerID identifies this worker in claimed_by (observability: which
	// replica ran a job). Empty is fine — the lease, not the identity, is
	// what protects against double-execution.
	WorkerID string
}

// NewPostgresJobStore returns a JobStore backed by db.
func NewPostgresJobStore(db *sql.DB) PostgresJobStore { return PostgresJobStore{DB: db} }

const jobColumns = `id::text, account_id::text, COALESCE(saved_report_id::text, ''), name,
       query_config::text, format, delivery, COALESCE(recipient, ''), source, status,
       COALESCE(error, ''), attempts, COALESCE(requested_by::text, ''),
       COALESCE(artifact_bucket, ''), COALESCE(artifact_key, ''),
       COALESCE(artifact_bytes, 0), COALESCE(row_count, 0),
       created_at, started_at, finished_at, expires_at`

func scanJob(scan func(dest ...any) error) (*Job, error) {
	var j Job
	var queryJSON string
	var started, finished sql.NullTime
	if err := scan(&j.ID, &j.AccountID, &j.SavedReportID, &j.Name,
		&queryJSON, &j.Format, &j.Delivery, &j.Recipient, &j.Source, &j.Status,
		&j.Error, &j.Attempts, &j.RequestedBy,
		&j.ArtifactBucket, &j.ArtifactKey, &j.ArtifactBytes, &j.RowCount,
		&j.CreatedAt, &started, &finished, &j.ExpiresAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(queryJSON), &j.QueryConfig); err != nil {
		return nil, fmt.Errorf("decode query_config for %s: %w", j.ID, err)
	}
	if started.Valid {
		t := started.Time
		j.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time
		j.FinishedAt = &t
	}
	return &j, nil
}

// Enqueue inserts a queued job. The RLS context is set to the job's account so
// the insert passes tenant policy under a non-bypass role. For source=schedule
// the partial unique index makes it idempotent — an already-active job for the
// same saved report returns ("", nil).
func (s PostgresJobStore) Enqueue(ctx context.Context, j Job) (string, error) {
	if s.DB == nil {
		return "", sql.ErrConnDone
	}
	queryJSON, err := json.Marshal(j.QueryConfig)
	if err != nil {
		return "", fmt.Errorf("encode query_config: %w", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('app.current_account_id', $1, true)`, j.AccountID); err != nil {
		return "", err
	}
	var savedReportID, recipient, requestedBy any
	if j.SavedReportID != "" {
		savedReportID = j.SavedReportID
	}
	if j.Recipient != "" {
		recipient = j.Recipient
	}
	if j.RequestedBy != "" {
		requestedBy = j.RequestedBy
	}
	var expiresArg any
	if !j.ExpiresAt.IsZero() {
		expiresArg = j.ExpiresAt
	}
	q := `INSERT INTO report_jobs
	        (account_id, saved_report_id, name, query_config, format, delivery,
	         recipient, source, requested_by, expires_at)
	      VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::uuid,
	              COALESCE($10::timestamptz, now() + interval '30 days'))`
	if j.Source == SourceSchedule {
		q += ` ON CONFLICT (saved_report_id) WHERE source = 'schedule'
		         AND status IN ('queued', 'running') DO NOTHING`
	}
	q += ` RETURNING id::text`
	var id string
	err = tx.QueryRowContext(ctx, q,
		j.AccountID, savedReportID, j.Name, queryJSON, j.Format, j.Delivery,
		recipient, j.Source, requestedBy, expiresArg).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) { // schedule dedupe hit
		return "", tx.Commit()
	}
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// LeaseTTL is how long a claim is valid without a heartbeat. Executing
// workers extend it (ExtendLease) every ~third of this; a lapsed lease means
// the claiming worker is genuinely gone and ANY worker may reclaim the job.
// This is what makes the runner safe at N replicas — the old model requeued
// every 'running' job at boot, which assumed a single worker.
const LeaseTTL = 2 * time.Minute

// ClaimOne claims the oldest queued job in a single statement — the subselect's
// FOR UPDATE SKIP LOCKED means concurrent claimers take distinct rows. The
// claim carries a lease + claimant identity.
func (s PostgresJobStore) ClaimOne(ctx context.Context) (*Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	// The worker claims ANY tenant's queued job — inherently cross-tenant, so it
	// runs under the platform hatch (security #77). The report_jobs
	// tenant_isolation policy is USING-only (no separate WITH CHECK), so
	// platform_read admits the UPDATE too.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, err
	}
	row := tx.QueryRowContext(ctx, `
UPDATE report_jobs
SET status = 'running', started_at = now(), attempts = attempts + 1,
    lease_expires_at = now() + $1::interval, claimed_by = $2
WHERE id = (SELECT id FROM report_jobs WHERE status = 'queued'
            ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING `+jobColumns,
		fmt.Sprintf("%f seconds", LeaseTTL.Seconds()), s.WorkerID)
	j, err := scanJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return j, tx.Commit()
}

// execPlatform runs a single cross-tenant worker UPDATE under the platform hatch
// (security #77). The worker legitimately touches any tenant's job; report_jobs'
// tenant_isolation policy is USING-only (no separate WITH CHECK), so
// platform_read admits the write. NEVER use for request-path writes.
func (s PostgresJobStore) execPlatform(ctx context.Context, query string, args ...any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// ExtendLease heartbeats a running job's lease. A worker that dies stops
// extending; the lease lapses; ReclaimExpired hands the job to a peer.
func (s PostgresJobStore) ExtendLease(ctx context.Context, id string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	return s.execPlatform(ctx, `
UPDATE report_jobs SET lease_expires_at = now() + $1::interval
WHERE id = $2::uuid AND status = 'running'`,
		fmt.Sprintf("%f seconds", LeaseTTL.Seconds()), id)
}

// MarkDone records a successful run and its artifact.
func (s PostgresJobStore) MarkDone(ctx context.Context, id string, a Artifact) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	return s.execPlatform(ctx, `
UPDATE report_jobs
SET status = 'done', finished_at = now(), error = NULL,
    artifact_bucket = $2, artifact_key = $3, artifact_bytes = $4, row_count = $5
WHERE id = $1::uuid`, id, a.Bucket, a.Key, a.Bytes, a.Rows)
}

// MarkFailed records a failed run.
func (s PostgresJobStore) MarkFailed(ctx context.Context, id, errMsg string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	return s.execPlatform(ctx, `
UPDATE report_jobs SET status = 'failed', finished_at = now(), error = $2
WHERE id = $1::uuid`, id, errMsg)
}

// ListByAccount returns the account's jobs, newest first. Explicit tenant
// filter per convention (RLS is the safety net, not the mechanism).
func (s PostgresJobStore) ListByAccount(ctx context.Context, accountID string, limit int) ([]Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	if limit <= 0 {
		limit = 50
	}
	// Tenant-scoped: set the caller's account GUC so RLS admits their rows under
	// the NOBYPASSRLS app role (security #77). Read-only tx keeps the *sql.Rows
	// valid while scanning and auto-resets the GUC.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM report_jobs
		 WHERE account_id = $1::uuid ORDER BY created_at DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// GetByAccount returns one job only if it belongs to the account.
func (s PostgresJobStore) GetByAccount(ctx context.Context, accountID, id string) (*Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	// Tenant-scoped: set the caller's account GUC so RLS admits the row under the
	// NOBYPASSRLS app role (security #77). scan runs inside the read-only tx.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return nil, err
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM report_jobs
		 WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	j, err := scanJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// Expired returns done/failed jobs past their expires_at, for the sweeper.
func (s PostgresJobStore) Expired(ctx context.Context, now time.Time, limit int) ([]Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	if limit <= 0 {
		limit = 100
	}
	// Cross-tenant sweeper read → platform hatch (held open while scanning).
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM report_jobs
		 WHERE expires_at < $1 AND status IN ('done', 'failed')
		 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Delete removes a job row. Called by the gateway handler only after
// GetByAccount has confirmed ownership, and by the expiry sweeper — both
// legitimately cross-tenant, so it runs under the platform hatch (security #77).
func (s PostgresJobStore) Delete(ctx context.Context, id string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	return s.execPlatform(ctx, `DELETE FROM report_jobs WHERE id = $1::uuid`, id)
}

// ReclaimExpired flips running jobs whose LEASE lapsed back to queued.
// Replaces the boot-time started_at requeue: lease expiry proves the claimant
// is dead (it stopped heartbeating), so this is safe to run from ANY replica
// at ANY time — boot and periodically. Jobs with live leases are never
// touched, however long they run.
func (s PostgresJobStore) ReclaimExpired(ctx context.Context) (int, error) {
	if s.DB == nil {
		return 0, sql.ErrConnDone
	}
	// Cross-tenant worker sweep → platform hatch (security #77).
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
UPDATE report_jobs SET status = 'queued', started_at = NULL, lease_expires_at = NULL, claimed_by = NULL
WHERE status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at < now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}
