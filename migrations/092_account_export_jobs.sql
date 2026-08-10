-- +goose Up
-- Phase 11 (Business Operations) item 105: Account Closure and Data Export —
-- slice 2, the data-export package. When an owner requests their account's data
-- export, a job is enqueued here; the report-runner worker materializes the
-- account's campaigns / creatives / audiences (aggregate) / invoices / payouts /
-- audit log into CSVs + a metadata.json, zips them, and uploads the archive to
-- the private adtech-reports bucket. The gateway streams the download after auth
-- (the bucket is never public) until the link expires.
CREATE TABLE account_export_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued', 'running', 'done', 'failed')),
    artifact_bucket TEXT,
    artifact_key    TEXT,
    artifact_bytes  BIGINT,
    error           TEXT,
    attempts        INT NOT NULL DEFAULT 0,
    requested_by    TEXT,        -- JWT subject of the requester (e.g. "user-<uuid>")
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    -- Download-link lifetime; the sweeper deletes the artifact after this.
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT now() + interval '7 days'
);

-- At most one in-flight export per account (so repeated GET /export coalesces
-- onto the running job rather than piling up).
CREATE UNIQUE INDEX idx_account_export_inflight ON account_export_jobs (account_id)
    WHERE status IN ('queued', 'running');
CREATE INDEX idx_account_export_account ON account_export_jobs (account_id, created_at DESC);
CREATE INDEX idx_account_export_claim ON account_export_jobs (created_at) WHERE status = 'queued';

-- RLS: the account sees its own export jobs; the report-runner worker claims +
-- completes cross-tenant under the platform hatch. NULLIF empty-safe (mig 087).
ALTER TABLE account_export_jobs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON account_export_jobs
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE account_export_jobs;
