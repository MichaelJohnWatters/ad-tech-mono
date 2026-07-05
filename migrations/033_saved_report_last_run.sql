-- +goose Up
-- Track when a scheduled report last ran so the report runner can tell which
-- reports are due without re-running them every tick. NULL = never run.
ALTER TABLE saved_reports ADD COLUMN last_run_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE saved_reports DROP COLUMN last_run_at;
