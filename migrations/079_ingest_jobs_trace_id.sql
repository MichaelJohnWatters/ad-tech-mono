-- +goose Up

-- The uploader's REAL request trace (32-hex OTel), snapshotted at enqueue.
-- Before this column only INLINE uploads carried the request trace into
-- profile_signals.trace_id (Process ran inside the HTTP request context); an
-- API upload pushed async (large file or future run_at) lost it — the worker
-- has no request context. Whether an upload runs inline or async is a
-- threshold, not a semantic difference, so the lineage must not depend on it.
-- Empty for drop-zone jobs, which genuinely have no originating request.
ALTER TABLE audience_ingest_jobs ADD COLUMN trace_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE audience_ingest_jobs DROP COLUMN trace_id;
