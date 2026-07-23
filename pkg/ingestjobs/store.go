package ingestjobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// PostgresIngestStore is the audience_ingest_jobs queue. Tenant-facing writes
// set the RLS context; worker-facing reads span all tenants (a platform job).
// The dev Postgres role bypasses RLS; a prod deployment needs a service role
// permitted to read every audience_ingest_jobs row.
type PostgresIngestStore struct {
	DB *sql.DB
	// WorkerID identifies this worker in claimed_by (observability). Empty is
	// fine — the lease, not the identity, protects against double-execution.
	WorkerID string
}

// NewPostgresIngestStore returns a Store backed by db.
func NewPostgresIngestStore(db *sql.DB) PostgresIngestStore { return PostgresIngestStore{DB: db} }

const jobColumns = `id::text, account_id::text, source, COALESCE(provider, ''),
       COALESCE(provider_id::text, ''),
       file_bucket, file_key, segment_spec::text, run_at, status,
       attempts, max_attempts, COALESCE(error, ''),
       COALESCE(segment_id::text, ''), COALESCE(total_rows, 0), COALESCE(valid_rows, 0),
       COALESCE(rejected_rows, 0), COALESCE(matched_rows, 0), COALESCE(match_rate, 0),
       COALESCE(rejected_key, ''), created_at, started_at, finished_at,
       COALESCE(notify_emails, '{}')`

func scanJob(scan func(dest ...any) error) (*Job, error) {
	var j Job
	var specJSON string
	var started, finished sql.NullTime
	var notify pq.StringArray
	if err := scan(&j.ID, &j.AccountID, &j.Source, &j.Provider,
		&j.ProviderID,
		&j.FileBucket, &j.FileKey, &specJSON, &j.RunAt, &j.Status,
		&j.Attempts, &j.MaxAttempts, &j.Error,
		&j.SegmentID, &j.TotalRows, &j.ValidRows,
		&j.RejectedRows, &j.MatchedRows, &j.MatchRate,
		&j.RejectedKey, &j.CreatedAt, &started, &finished, &notify); err != nil {
		return nil, err
	}
	if len(notify) > 0 {
		j.NotifyEmails = []string(notify)
	}
	if err := json.Unmarshal([]byte(specJSON), &j.SegmentSpec); err != nil {
		return nil, fmt.Errorf("decode segment_spec for %s: %w", j.ID, err)
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
// the insert passes tenant policy under a non-bypass role. The partial unique
// index on (file_bucket, file_key) makes it idempotent per staged file — an
// already-active job for the same file returns ("", nil).
func (s PostgresIngestStore) Enqueue(ctx context.Context, j Job) (string, error) {
	if s.DB == nil {
		return "", sql.ErrConnDone
	}
	specJSON, err := json.Marshal(j.SegmentSpec)
	if err != nil {
		return "", fmt.Errorf("encode segment_spec: %w", err)
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
	var provider any
	if j.Provider != "" {
		provider = j.Provider
	}
	var providerID any
	if j.ProviderID != "" {
		providerID = j.ProviderID
	}
	var runAt any
	if !j.RunAt.IsZero() {
		runAt = j.RunAt
	}
	maxAttempts := j.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	q := `INSERT INTO audience_ingest_jobs
	        (account_id, source, provider, provider_id, file_bucket, file_key, segment_spec,
	         run_at, max_attempts, notify_emails)
	      VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7::jsonb,
	              COALESCE($8::timestamptz, now()), $9, $10::text[])
	      ON CONFLICT (file_bucket, file_key) WHERE status IN ('queued', 'running')
	        DO NOTHING
	      RETURNING id::text`
	var id string
	err = tx.QueryRowContext(ctx, q,
		j.AccountID, j.Source, provider, providerID, j.FileBucket, j.FileKey, string(specJSON),
		runAt, maxAttempts, pq.Array(j.NotifyEmails)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) { // dedupe hit
		return "", tx.Commit()
	}
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// LeaseTTL is how long a claim is valid without a heartbeat. Executing workers
// extend it (ExtendLease) every ~third of this; a lapsed lease means the
// claiming worker is genuinely gone and ANY worker may reclaim the job.
const LeaseTTL = 2 * time.Minute

// ClaimOne claims the oldest DUE queued job in a single statement — the
// subselect's FOR UPDATE SKIP LOCKED means concurrent claimers take distinct
// rows. run_at <= now() gates scheduled/held files. The claim carries a lease +
// claimant identity.
func (s PostgresIngestStore) ClaimOne(ctx context.Context) (*Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	row := s.DB.QueryRowContext(ctx, `
UPDATE audience_ingest_jobs
SET status = 'running', started_at = now(), attempts = attempts + 1,
    lease_expires_at = now() + $1::interval, claimed_by = $2
WHERE id = (SELECT id FROM audience_ingest_jobs
            WHERE status = 'queued' AND run_at <= now()
            ORDER BY run_at, created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING `+jobColumns,
		fmt.Sprintf("%f seconds", LeaseTTL.Seconds()), s.WorkerID)
	j, err := scanJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ClaimByID claims a specific queued job by id — the same lease + attempts++
// as ClaimOne, but WHERE id=$1 AND status='queued'. It ignores run_at (the
// caller has decided this row is due-now). Returns (nil, nil) when the row is
// absent or no longer queued (already claimed/done). The gateway inline path
// claims the row it just enqueued so its run holds the worker's lease, making a
// crashed inline job recoverable by the worker.
func (s PostgresIngestStore) ClaimByID(ctx context.Context, id string) (*Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	row := s.DB.QueryRowContext(ctx, `
UPDATE audience_ingest_jobs
SET status = 'running', started_at = now(), attempts = attempts + 1,
    lease_expires_at = now() + $1::interval, claimed_by = $2
WHERE id = $3::uuid AND status = 'queued'
RETURNING `+jobColumns,
		fmt.Sprintf("%f seconds", LeaseTTL.Seconds()), s.WorkerID, id)
	j, err := scanJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ExtendLease heartbeats a running job's lease. A worker that dies stops
// extending; the lease lapses; ReclaimExpired hands the job to a peer.
func (s PostgresIngestStore) ExtendLease(ctx context.Context, id string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE audience_ingest_jobs SET lease_expires_at = now() + $1::interval
WHERE id = $2::uuid AND status = 'running'`,
		fmt.Sprintf("%f seconds", LeaseTTL.Seconds()), id)
	return err
}

// MarkDone records a successful run and its result counts.
func (s PostgresIngestStore) MarkDone(ctx context.Context, id string, r IngestResult) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	var segID any
	if r.SegmentID != "" {
		segID = r.SegmentID
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE audience_ingest_jobs
SET status = 'done', finished_at = now(), error = NULL,
    segment_id = $2::uuid, total_rows = $3, valid_rows = $4, rejected_rows = $5,
    matched_rows = $6, match_rate = $7, rejected_key = NULLIF($8, '')
WHERE id = $1::uuid`,
		id, segID, r.TotalRows, r.ValidRows, r.RejectedRows,
		r.MatchedRows, r.MatchRate, r.RejectedKey)
	return err
}

// MarkFailed records a failed run.
func (s PostgresIngestStore) MarkFailed(ctx context.Context, id, errMsg string) error {
	if s.DB == nil {
		return sql.ErrConnDone
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE audience_ingest_jobs SET status = 'failed', finished_at = now(), error = $2
WHERE id = $1::uuid`, id, errMsg)
	return err
}

// ListByAccount returns the account's jobs, newest first. Explicit tenant
// filter per convention (RLS is the safety net, not the mechanism).
func (s PostgresIngestStore) ListByAccount(ctx context.Context, accountID string, limit int) ([]Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM audience_ingest_jobs
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
func (s PostgresIngestStore) GetByAccount(ctx context.Context, accountID, id string) (*Job, error) {
	if s.DB == nil {
		return nil, sql.ErrConnDone
	}
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM audience_ingest_jobs
		 WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	j, err := scanJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ReclaimExpired flips running jobs whose LEASE lapsed back to queued. Lease
// expiry proves the claimant is dead (it stopped heartbeating), so this is safe
// to run from ANY replica at ANY time — boot and periodically. Jobs with live
// leases are never touched, however long they run.
func (s PostgresIngestStore) ReclaimExpired(ctx context.Context) (int, error) {
	if s.DB == nil {
		return 0, sql.ErrConnDone
	}
	res, err := s.DB.ExecContext(ctx, `
UPDATE audience_ingest_jobs
SET status = 'queued', started_at = NULL, lease_expires_at = NULL, claimed_by = NULL
WHERE status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at < now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
