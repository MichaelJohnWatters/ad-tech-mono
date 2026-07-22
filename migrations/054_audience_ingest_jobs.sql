-- +goose Up

-- audience_ingest_jobs — one durable queue for ALL audience file ingestion
-- (ADR 0007). Mirrors report_jobs (Postgres-as-queue, FOR UPDATE SKIP LOCKED +
-- lease-based requeue), but adds a run_at scheduling gate and folds the terminal
-- result counts that used to live in onboarding_runs onto the job row itself.
--
-- Two producers stage a file to S3 then insert one row: the 3rd-party drop-zone
-- poller (source='dropzone') and, in a later phase, the gateway upload
-- (source='api'). One processor (pkg/pipeline processStagedFile) drains it. The
-- pipeline ingest worker claims queued+due rows; the gateway may inline-run a
-- small, due-now row under the SAME lease.
CREATE TABLE audience_ingest_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- Which producer staged the file.
    source TEXT NOT NULL CHECK (source IN ('api', 'dropzone')),
    -- 3rd-party provider name (drop-zone); NULL for 'api' uploads.
    provider TEXT,
    -- The staged source file in object storage.
    file_bucket TEXT NOT NULL,
    file_key TEXT NOT NULL,
    -- Everything the processor needs that used to come from the per-provider
    -- manifest: name, type, visibility, consent, id_type, field_mappings,
    -- segment_name, access.
    segment_spec JSONB NOT NULL,
    -- Scheduling / hold_until gate: the claim query only takes rows with
    -- run_at <= now(). A held file is staged but never opened until its date.
    run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    attempts INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 5,
    -- Lease: a running job whose lease lapsed (claimant stopped heartbeating) is
    -- reclaimable by any worker. Safe at N replicas.
    lease_expires_at TIMESTAMPTZ,
    claimed_by TEXT,
    error TEXT,
    -- Terminal result counts (folds onboarding_runs).
    segment_id UUID,
    total_rows INT,
    valid_rows INT,
    rejected_rows INT,
    matched_rows INT,
    match_rate DOUBLE PRECISION,
    rejected_key TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);

-- Claim scan: queued rows that are due, oldest run_at first.
CREATE INDEX idx_audience_ingest_jobs_claim ON audience_ingest_jobs (status, run_at);
-- Account-facing listing.
CREATE INDEX idx_audience_ingest_jobs_account ON audience_ingest_jobs (account_id, created_at DESC);
-- A drop-zone file can't be enqueued twice while an ingest for it is in flight.
CREATE UNIQUE INDEX idx_audience_ingest_jobs_dedupe ON audience_ingest_jobs (file_bucket, file_key)
    WHERE status IN ('queued', 'running');

ALTER TABLE audience_ingest_jobs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audience_ingest_jobs
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON audience_ingest_jobs;
DROP TABLE audience_ingest_jobs;
