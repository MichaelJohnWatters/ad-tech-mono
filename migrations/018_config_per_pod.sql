-- +goose Up
-- Per-pod config rows for the tier model.
--
-- The config manager now stores one row per (pod_id, key) instead of one row
-- per key. This lets each pod own its tunable values cleanly:
--   - registry.Register seeds defaults for the pod's own pod_id only
--   - operator edits target a single pod (or fan out via "Save All")
--   - dead pods leave orphaned rows that the UI alert surfaces
--
-- Existing rows are migrated to pod_id = '' (the legacy "global" fallback);
-- the read path falls back to '' if no row exists for the requesting pod.
-- This keeps the cutover safe — old pods reading the table during the
-- migration window still see their values.

ALTER TABLE config ADD COLUMN pod_id TEXT NOT NULL DEFAULT '';

-- Composite primary key. Old PK on key alone is dropped; same key can now
-- have distinct rows per pod_id.
ALTER TABLE config DROP CONSTRAINT config_pkey;
ALTER TABLE config ADD PRIMARY KEY (pod_id, key);

-- Index for the common lookup: "give me this key for this pod (or global)".
CREATE INDEX idx_config_key ON config (key);

-- +goose Down
DROP INDEX IF EXISTS idx_config_key;
ALTER TABLE config DROP CONSTRAINT config_pkey;
-- Restoring the old PK requires removing pod-specific duplicates first.
DELETE FROM config a USING config b
    WHERE a.key = b.key AND a.pod_id <> '' AND b.pod_id = ''
       OR (a.key = b.key AND a.ctid > b.ctid);
ALTER TABLE config ADD PRIMARY KEY (key);
ALTER TABLE config DROP COLUMN pod_id;
