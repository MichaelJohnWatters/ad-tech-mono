// Package reportjobs is the async report builder: report queries submitted as
// jobs (from a saved-report template or ad-hoc params), claimed by the
// report-runner worker, executed through the reporting query API, and stored
// as downloadable artifacts (CSV/JSON/Parquet) in object storage.
//
// Postgres is the queue — jobs are rows in report_jobs claimed with
// FOR UPDATE SKIP LOCKED. Report volume is low (human-initiated + interval
// schedules), so a broker would be overkill; a Postgres row also IS the job
// status the portal polls, with no state to reconcile.
package reportjobs

import (
	"context"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// Job statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Job sources.
const (
	SourceManual   = "manual"
	SourceSchedule = "schedule"
)

// Artifact formats.
const (
	FormatCSV     = "csv"
	FormatJSON    = "json"
	FormatParquet = "parquet"
)

// Deliveries.
const (
	DeliveryEmail = "email"
	// DeliveryWebhook delivers via the account's webhook subscriptions (the
	// executor publishes report.completed; the dispatcher does the POSTs).
	DeliveryWebhook = "webhook"
	DeliveryNone    = "none"
)

// ValidFormat reports whether f is a supported artifact format.
func ValidFormat(f string) bool {
	return f == FormatCSV || f == FormatJSON || f == FormatParquet
}

// Job is one async report execution: the query snapshot, its lifecycle state,
// and (once done) the artifact location.
type Job struct {
	ID            string                `json:"id"`
	AccountID     string                `json:"account_id"`
	SavedReportID string                `json:"saved_report_id,omitempty"` // "" for ad-hoc
	Name          string                `json:"name"`
	QueryConfig   analytics.QueryParams `json:"query_config"`
	Format        string                `json:"format"`
	Delivery      string                `json:"delivery"`
	Recipient     string                `json:"recipient,omitempty"`
	Source        string                `json:"source"`
	Status        string                `json:"status"`
	Error         string                `json:"error,omitempty"`
	Attempts      int                   `json:"attempts"`
	RequestedBy   string                `json:"requested_by,omitempty"`

	ArtifactBucket string `json:"-"`
	ArtifactKey    string `json:"-"`
	ArtifactBytes  int64  `json:"artifact_bytes,omitempty"`
	RowCount       int64  `json:"row_count,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

// Artifact is the stored result of a completed job.
type Artifact struct {
	Bucket string
	Key    string
	Bytes  int64
	Rows   int64
}

// JobStore is the queue + job registry. Enqueue/List/Get are tenant-facing
// (gateway); Claim/Lease/Mark/Expired/Delete/Reclaim are worker-facing.
type JobStore interface {
	// Enqueue inserts a queued job and returns its id. For source=schedule it
	// is idempotent per active saved report: if a queued/running job for the
	// same saved_report_id already exists it returns ("", nil).
	Enqueue(ctx context.Context, j Job) (string, error)
	// ClaimOne atomically claims the oldest queued job, marking it running.
	// Returns (nil, nil) when the queue is empty.
	ClaimOne(ctx context.Context) (*Job, error)
	// MarkDone records a successful run and its artifact.
	MarkDone(ctx context.Context, id string, a Artifact) error
	// MarkFailed records a failed run.
	MarkFailed(ctx context.Context, id, errMsg string) error
	// ListByAccount returns the account's jobs, newest first.
	ListByAccount(ctx context.Context, accountID string, limit int) ([]Job, error)
	// GetByAccount returns one job only if it belongs to the account
	// (nil, nil when absent — callers 404).
	GetByAccount(ctx context.Context, accountID, id string) (*Job, error)
	// Expired returns done/failed jobs whose expires_at has passed.
	Expired(ctx context.Context, now time.Time, limit int) ([]Job, error)
	// Delete removes a job row (the sweeper deletes the artifact first).
	Delete(ctx context.Context, id string) error
	// ExtendLease heartbeats a running job's lease; a lapsed lease marks
	// the claimant dead and the job reclaimable by any worker.
	ExtendLease(ctx context.Context, id string) error

	// ReclaimExpired flips running jobs with LAPSED leases back to queued —
	// crash recovery that is safe at any replica count (boot + periodic).
	ReclaimExpired(ctx context.Context) (int, error)
}
