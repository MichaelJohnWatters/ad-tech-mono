-- +goose Up
-- Per-service config schema published at boot. Replaces the hardcoded slice
-- in pkg/config/schema.go so each service owns its own keys — adding a new
-- DSP knob no longer requires editing a shared package. The config-manager
-- UI reads the union of every service's published rows from this table.
--
-- Each service UPSERTs all its rows on boot; service column lets us replace
-- a service's full schema in one transaction (delete service's rows, insert
-- fresh) so removing a deprecated key actually removes it from the UI.

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

-- +goose Down
DROP INDEX IF EXISTS idx_config_schema_service;
DROP TABLE config_schema;
