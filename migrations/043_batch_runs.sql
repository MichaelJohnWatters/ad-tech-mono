-- +goose Up

-- Batch-conductor run log: one row per STEP per chain run. The conductor
-- (cmd/batch-conductor) replaced the time-staggered CronJob lattice with an
-- explicit completion-chained sequence (checkpoint → compact → rollups →
-- profile-builder → privacy delete → verify); this table is the record of
-- what ran, in what order, with what outcome — the observability that
-- schedule offsets never provided. Feeds the staff "Batch runs" page.
--
-- Platform-global operational telemetry (no RLS) — same posture as
-- onboarding_runs: written by a platform job, read by staff.
CREATE TABLE batch_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL,
    seq INT NOT NULL,
    step TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('running', 'done', 'failed', 'skipped')),
    critical BOOLEAN NOT NULL DEFAULT false,
    detail TEXT,
    error TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ
);

CREATE INDEX idx_batch_runs_run ON batch_runs (run_id, seq);
CREATE INDEX idx_batch_runs_recent ON batch_runs (started_at DESC);

-- +goose Down
DROP TABLE batch_runs;
