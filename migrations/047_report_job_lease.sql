-- +goose Up
-- Per-job lease: converts report-runner crash recovery from "boot-time
-- requeue of anything running" (safe only with EXACTLY ONE worker — a second
-- replica booting would requeue jobs the first is actively executing) into
-- lease-expiry reclaim, which is safe at any replica count. Workers heartbeat
-- the lease while executing; a job whose lease lapses is genuinely orphaned
-- and any worker may reclaim it.
ALTER TABLE report_jobs ADD COLUMN lease_expires_at TIMESTAMPTZ;
ALTER TABLE report_jobs ADD COLUMN claimed_by TEXT;

-- +goose Down
ALTER TABLE report_jobs DROP COLUMN lease_expires_at;
ALTER TABLE report_jobs DROP COLUMN claimed_by;
