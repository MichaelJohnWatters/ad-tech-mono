-- +goose Up

-- Async report jobs: a queued unit of report work (from a saved-report template
-- or ad-hoc params) claimed by the report-runner worker, executed through the
-- reporting query API, and materialised as a downloadable artifact (CSV/JSON/
-- Parquet) in object storage. Postgres is the queue: workers claim with
-- FOR UPDATE SKIP LOCKED — report volume is low, so no dedicated broker.
CREATE TABLE report_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    saved_report_id UUID REFERENCES saved_reports(id) ON DELETE SET NULL,
    name TEXT NOT NULL,
    -- Tenant-scoped analytics.QueryParams snapshot (scope filters resolved at
    -- enqueue time so the worker never re-derives tenancy).
    query_config JSONB NOT NULL,
    format TEXT NOT NULL DEFAULT 'csv' CHECK (format IN ('csv', 'json', 'parquet')),
    delivery TEXT NOT NULL DEFAULT 'none' CHECK (delivery IN ('email', 'none')),
    recipient TEXT, -- resolved at enqueue when delivery='email'
    source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'schedule')),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    error TEXT,
    attempts INT NOT NULL DEFAULT 0,
    requested_by UUID REFERENCES team_members(id) ON DELETE SET NULL,
    artifact_bucket TEXT,
    artifact_key TEXT,
    artifact_bytes BIGINT,
    row_count BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    -- SQL default is a fallback; the inserting code sets it from the
    -- report_runner.retention config key.
    expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + interval '30 days'
);

CREATE INDEX idx_report_jobs_account ON report_jobs (account_id, created_at DESC);
-- Claim scan: only queued rows, oldest first.
CREATE INDEX idx_report_jobs_claim ON report_jobs (created_at) WHERE status = 'queued';
-- Retention sweep.
CREATE INDEX idx_report_jobs_expiry ON report_jobs (expires_at)
    WHERE status IN ('done', 'failed');
-- Scheduler-enqueue idempotence: at most one active job per scheduled saved
-- report, so a scheduler tick that raced a crash can ON CONFLICT DO NOTHING.
CREATE UNIQUE INDEX idx_report_jobs_sched_active ON report_jobs (saved_report_id)
    WHERE source = 'schedule' AND status IN ('queued', 'running');

ALTER TABLE report_jobs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON report_jobs
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- Scheduled reports produce an artifact now, so each needs a format.
ALTER TABLE saved_reports ADD COLUMN format TEXT NOT NULL DEFAULT 'csv'
    CHECK (format IN ('csv', 'json', 'parquet'));

-- +goose Down
ALTER TABLE saved_reports DROP COLUMN format;
DROP POLICY IF EXISTS tenant_isolation ON report_jobs;
DROP TABLE report_jobs;
