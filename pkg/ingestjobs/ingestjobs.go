// Package ingestjobs is the durable audience-ingestion queue (ADR 0007): every
// audience file — 1st-party upload or 3rd-party drop-zone drop — is staged to
// object storage and recorded as one row in audience_ingest_jobs, claimed with
// FOR UPDATE SKIP LOCKED and a lease. One processor (pkg/pipeline
// processStagedFile) drains it; the job row carries both the queue state and
// the terminal result counts (folding the old onboarding_runs table).
//
// It mirrors pkg/reportjobs, with two differences that matter: a run_at
// scheduling gate (the claim only takes due rows) and a (file_bucket, file_key)
// dedupe so the same staged file is never enqueued twice while in flight.
package ingestjobs

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

// Job sources — which producer staged the file.
const (
	SourceAPI      = "api"      // gateway upload (Phase 2)
	SourceDropzone = "dropzone" // 3rd-party partner drop-zone poller
)

// SegmentSpec carries everything the processor needs that used to be read from
// the per-provider drop-zone manifest at process time — snapshotted onto the
// job at enqueue so the file is self-describing when the worker claims it.
type SegmentSpec struct {
	// Name is the target segment name (derived from the file name for the
	// drop-zone; the caller-supplied list name for uploads).
	Name string `json:"name"`
	// Type is the segment type (e.g. cdp_imported). Empty → processor default.
	Type string `json:"type,omitempty"`
	// Visibility is public | dsp_private. Empty → processor default.
	Visibility string `json:"visibility,omitempty"`
	// Consent marks the signals consented for personalisation.
	Consent bool `json:"consent"`
	// IDType is stamped on rows without an explicit id_type column
	// (user_id | hashed_email | uid2 | device_id | household).
	IDType string `json:"id_type,omitempty"`
	// FieldMappings map provider CSV columns → canonical columns, merged over
	// the processor's defaults.
	FieldMappings map[string]string `json:"field_mappings,omitempty"`
	// RequiredFields validated per row. Empty → ["id_value"].
	RequiredFields []string `json:"required_fields,omitempty"`
	// Access records the licence (purchased:{provider} | barter:{provider} |
	// first_party). Empty → processor default.
	Access string `json:"access,omitempty"`
}

// IngestResult is what the processor returns for a completed ingest — the
// counts that get recorded on the job row via MarkDone.
type IngestResult struct {
	SegmentID    string
	TotalRows    int
	ValidRows    int
	RejectedRows int
	MatchedRows  int
	MatchRate    float64
	RejectedKey  string
}

// Job is one audience-ingestion unit: the staged file, the segment spec, its
// lifecycle state, and (once done) the terminal result counts.
type Job struct {
	ID          string      `json:"id"`
	AccountID   string      `json:"account_id"`
	Source      string      `json:"source"`
	Provider    string      `json:"provider,omitempty"`
	FileBucket  string      `json:"file_bucket"`
	FileKey     string      `json:"file_key"`
	SegmentSpec SegmentSpec `json:"segment_spec"`
	RunAt       time.Time   `json:"run_at"`
	Status      string      `json:"status"`
	Attempts    int         `json:"attempts"`
	MaxAttempts int         `json:"max_attempts"`
	Error       string      `json:"error,omitempty"`

	// Terminal result (populated on done).
	SegmentID    string  `json:"segment_id,omitempty"`
	TotalRows    int     `json:"total_rows,omitempty"`
	ValidRows    int     `json:"valid_rows,omitempty"`
	RejectedRows int     `json:"rejected_rows,omitempty"`
	MatchedRows  int     `json:"matched_rows,omitempty"`
	MatchRate    float64 `json:"match_rate,omitempty"`
	RejectedKey  string  `json:"rejected_key,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Store is the queue + job registry. Enqueue/ListByAccount/GetByAccount are
// tenant-facing; ClaimOne/ExtendLease/MarkDone/MarkFailed/ReclaimExpired are
// worker-facing (they span tenants — a platform job).
type Store interface {
	// Enqueue inserts a queued job and returns its id. It is idempotent per
	// staged file: if a queued/running job for the same (file_bucket, file_key)
	// already exists it returns ("", nil).
	Enqueue(ctx context.Context, j Job) (string, error)
	// ClaimOne atomically claims the oldest DUE queued job (run_at <= now),
	// marking it running with a lease. Returns (nil, nil) when none are ready.
	ClaimOne(ctx context.Context) (*Job, error)
	// ExtendLease heartbeats a running job's lease; a lapsed lease marks the
	// claimant dead and the job reclaimable by any worker.
	ExtendLease(ctx context.Context, id string) error
	// MarkDone records a successful run and its result counts.
	MarkDone(ctx context.Context, id string, r IngestResult) error
	// MarkFailed records a failed run.
	MarkFailed(ctx context.Context, id, errMsg string) error
	// ListByAccount returns the account's jobs, newest first.
	ListByAccount(ctx context.Context, accountID string, limit int) ([]Job, error)
	// GetByAccount returns one job only if it belongs to the account
	// (nil, nil when absent — callers 404).
	GetByAccount(ctx context.Context, accountID, id string) (*Job, error)
	// ReclaimExpired flips running jobs with LAPSED leases back to queued —
	// crash recovery safe at any replica count (boot + periodic).
	ReclaimExpired(ctx context.Context) (int, error)
}
