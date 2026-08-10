// Package accountexport is the account data-export package (PLAN Phase 11, item
// 105: Account Closure and Data Export — slice 2). An owner requests an export;
// a job is enqueued and the report-runner worker materializes the account's data
// (campaigns, creatives, audiences aggregate, invoices, payouts, audit log) into
// CSVs + a metadata.json, zips them, and uploads the archive to the private
// reports bucket. The gateway streams the download after auth.
//
// SCOPE NOTE (documented simplification): the export carries structured data as
// CSV — creative metadata (name, dimensions, landing url), not the binary
// creative assets; invoices/payouts as CSV, not rendered PDFs; audiences as
// aggregate counts, never member ids (privacy). The PLAN's richer package
// (asset zip from S3, PDF invoices) is a later hardening.
package accountexport

import (
	"context"
	"time"
)

// Job statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// DefaultTTL is how long a completed export's download link stays live.
const DefaultTTL = 7 * 24 * time.Hour

// Job is one account-export request.
type Job struct {
	ID             string     `json:"id"`
	AccountID      string     `json:"account_id"`
	Status         string     `json:"status"`
	ArtifactBucket string     `json:"-"`
	ArtifactKey    string     `json:"-"`
	ArtifactBytes  int64      `json:"artifact_bytes,omitempty"`
	Error          string     `json:"error,omitempty"`
	RequestedBy    string     `json:"requested_by,omitempty"`
	CreatedAt      time.Time  `json:"requested_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at"`
}

// Expired reports whether a completed export's download window has passed.
func (j Job) Expired(now time.Time) bool { return now.After(j.ExpiresAt) }

// HasArtifact reports whether the job produced a downloadable archive.
func (j Job) HasArtifact() bool {
	return j.Status == StatusDone && j.ArtifactKey != ""
}

// Store persists account-export jobs.
type Store interface {
	// Enqueue creates a queued export for the account, or returns the existing
	// in-flight (queued/running) job if one is already open (the partial unique
	// index coalesces concurrent requests).
	Enqueue(ctx context.Context, accountID, requestedBy string, ttl time.Duration) (Job, error)
	// LatestForAccount returns the account's most recent export job, or nil.
	LatestForAccount(ctx context.Context, accountID string) (*Job, error)
	// Get returns one job by id, scoped to the account (nil when absent).
	Get(ctx context.Context, accountID, id string) (*Job, error)
	// ClaimNext atomically claims the oldest queued job (worker; cross-tenant via
	// the platform hatch), marking it running. Returns nil when the queue is empty.
	ClaimNext(ctx context.Context) (*Job, error)
	// MarkDone records a completed artifact (worker; platform hatch).
	MarkDone(ctx context.Context, id, bucket, key string, bytes int64) error
	// MarkFailed records a failure (worker; platform hatch).
	MarkFailed(ctx context.Context, id, errMsg string) error
}
