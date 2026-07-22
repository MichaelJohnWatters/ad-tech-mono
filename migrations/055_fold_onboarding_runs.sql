-- +goose Up

-- ADR 0007 Phase 4 — fold onboarding_runs into audience_ingest_jobs and drop it.
-- The job row already carries every count onboarding_runs held (source, provider,
-- file_bucket/file_key, segment_id, total/valid/rejected/matched rows, match_rate,
-- rejected_key, status, started_at, finished_at). The only column missing was the
-- retention-sweep bookkeeping flag, added here. The onboarding-monitor + retention
-- sweep now read the job table; onboarding_runs was a redundant second write.
--
-- Status value change: onboarding_runs used 'completed'; the job table uses 'done'.
ALTER TABLE audience_ingest_jobs ADD COLUMN swept_at TIMESTAMPTZ;

-- Sweep scan: terminal, unswept rows past the retention window.
CREATE INDEX idx_audience_ingest_jobs_sweep ON audience_ingest_jobs (finished_at)
    WHERE swept_at IS NULL AND status IN ('done', 'failed');

DROP TABLE onboarding_runs;

-- +goose Down

-- Recreate onboarding_runs exactly as migrations 041 + 044 left it.
CREATE TABLE onboarding_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL,
    file_key TEXT NOT NULL,
    account_id UUID,
    segment_id UUID,
    status TEXT NOT NULL CHECK (status IN ('completed', 'failed')),
    total_rows INT NOT NULL DEFAULT 0,
    valid_rows INT NOT NULL DEFAULT 0,
    rejected_rows INT NOT NULL DEFAULT 0,
    matched_rows INT NOT NULL DEFAULT 0,
    match_rate DOUBLE PRECISION,
    error TEXT,
    rejected_key TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    swept_at TIMESTAMPTZ
);

CREATE INDEX idx_onboarding_runs_provider ON onboarding_runs (provider, finished_at DESC);
CREATE INDEX idx_onboarding_runs_sweep ON onboarding_runs (finished_at)
    WHERE swept_at IS NULL;

DROP INDEX IF EXISTS idx_audience_ingest_jobs_sweep;
ALTER TABLE audience_ingest_jobs DROP COLUMN swept_at;
