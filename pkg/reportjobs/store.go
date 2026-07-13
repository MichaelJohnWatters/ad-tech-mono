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
type PostgresJobStore struct{ DB *sql.DB }

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

// ClaimOne claims the oldest queued job in a single statement — the subselect's
// FOR UPDATE SKIP LOCKED means concurrent claimers take distinct rows.
func (s PostgresJobStore) ClaimOne(ctx context.Context) (*Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	row := s.DB.QueryRowContext(ctx, `
UPDATE report_jobs
SET status = 'running', started_at = now(), attempts = attempts + 1
WHERE id = (SELECT id FROM report_jobs WHERE status = 'queued'
            ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING `+jobColumns)
	j, err := scanJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// MarkDone records a successful run and its artifact.
func (s PostgresJobStore) MarkDone(ctx context.Context, id string, a Artifact) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE report_jobs
SET status = 'done', finished_at = now(), error = NULL,
    artifact_bucket = $2, artifact_key = $3, artifact_bytes = $4, row_count = $5
WHERE id = $1::uuid`, id, a.Bucket, a.Key, a.Bytes, a.Rows)
	return err
}

// MarkFailed records a failed run.
func (s PostgresJobStore) MarkFailed(ctx context.Context, id, errMsg string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE report_jobs SET status = 'failed', finished_at = now(), error = $2
WHERE id = $1::uuid`, id, errMsg)
	return err
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
	rows, err := s.DB.QueryContext(ctx,
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
	row := s.DB.QueryRowContext(ctx,
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
	rows, err := s.DB.QueryContext(ctx,
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

// Delete removes a job row.
func (s PostgresJobStore) Delete(ctx context.Context, id string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM report_jobs WHERE id = $1::uuid`, id)
	return err
}

// RequeueStuck flips running jobs older than olderThan back to queued — crash
// recovery on worker boot (safe: single-replica worker).
func (s PostgresJobStore) RequeueStuck(ctx context.Context, olderThan time.Duration) (int, error) {
	if s.DB == nil {
		return 0, sql.ErrConnDone
	}
	res, err := s.DB.ExecContext(ctx, `
UPDATE report_jobs SET status = 'queued', started_at = NULL
WHERE status = 'running' AND started_at < now() - $1::interval`,
		fmt.Sprintf("%f seconds", olderThan.Seconds()))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
