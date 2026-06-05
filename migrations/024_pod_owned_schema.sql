-- +goose Up
-- Pod-owned schema. Each pod publishes its full []SchemaEntry into its own
-- service_registry row at boot, replacing the flat `config_keys` JSONB list
-- and the separate `config_schema` table.
--
-- Why the change: previously schema lived in two places (in-process Go
-- registry + the config_schema table), with a brittle boot order — the
-- pod registered its `config_keys` from whatever was in the in-process
-- registry at that moment, but per-service schemas were published *after*
-- registration. The DSP-owned keys (dsp.daily_budget_default etc.) showed
-- up in config_schema but not in the pod's `config_keys`, so the UI
-- rendered them as "this pod does not use this key".
--
-- New model: the pod is the source of truth. It writes the full schema
-- entry (key, type, default, description, tier, since) into
-- service_registry.schema_entries at register. The config-manager UI
-- aggregates by unioning schema_entries across every running pod — no
-- separate table, no boot-order ambiguity.

-- service_registry is normally created lazily by pkg/config/registry.go on
-- first pod boot. In a fresh cluster the migrate job runs before any service
-- starts, so create the table here if it doesn't exist yet. Shape mirrors
-- the Go-side definition.
CREATE TABLE IF NOT EXISTS service_registry (
    pod_id          TEXT NOT NULL,
    service         TEXT NOT NULL,
    version         TEXT,
    host            TEXT,
    port            TEXT,
    status          TEXT DEFAULT 'running',
    started_at      TIMESTAMPTZ DEFAULT now(),
    last_ping_at    TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (service, pod_id)
);

ALTER TABLE service_registry
    ADD COLUMN IF NOT EXISTS schema_entries JSONB NOT NULL DEFAULT '[]';

ALTER TABLE service_registry
    DROP COLUMN IF EXISTS config_keys;

DROP INDEX IF EXISTS idx_config_schema_service;
DROP TABLE IF EXISTS config_schema;

-- +goose Down
CREATE TABLE config_schema (
    key            TEXT PRIMARY KEY,
    type           TEXT NOT NULL,
    tier           TEXT NOT NULL,
    "default"      TEXT NOT NULL DEFAULT '',
    description    TEXT NOT NULL DEFAULT '',
    service        TEXT NOT NULL,
    since_version  TEXT NOT NULL DEFAULT '',
    deprecated     BOOLEAN NOT NULL DEFAULT false,
    replaced_by    TEXT NOT NULL DEFAULT '',
    published_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_config_schema_service ON config_schema (service);

ALTER TABLE service_registry
    ADD COLUMN IF NOT EXISTS config_keys JSONB DEFAULT '[]';

ALTER TABLE service_registry
    DROP COLUMN IF EXISTS schema_entries;
