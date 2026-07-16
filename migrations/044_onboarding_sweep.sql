-- +goose Up

-- Retention sweep bookkeeping for drop-zone artifacts. After a file is
-- ingested, its original (still-compressed) bytes live on in
-- {provider}/processed/ (audit/debug copy) and rejects in
-- {provider}/rejected/ — previously forever. The poller now deletes both
-- once the run is older than pipeline.onboarding_retention and stamps
-- swept_at, so the monitor can still show the run's stats after its bytes
-- are gone.
ALTER TABLE onboarding_runs ADD COLUMN swept_at TIMESTAMPTZ;

CREATE INDEX idx_onboarding_runs_sweep ON onboarding_runs (finished_at)
    WHERE swept_at IS NULL;

-- +goose Down
DROP INDEX idx_onboarding_runs_sweep;
ALTER TABLE onboarding_runs DROP COLUMN swept_at;
