-- +goose Up
-- API changelog (PLAN Phase 11, item 109): a public, staff-authored log of API
-- changes for external integrators. Platform-global (no account_id / no RLS —
-- like incidents, mig 093): staff write, everyone reads.
CREATE TABLE changelog_entries (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version            TEXT NOT NULL,                 -- e.g. "1.2.0"
    release_date       DATE NOT NULL,
    category           TEXT NOT NULL DEFAULT 'changed'
                         CHECK (category IN ('added', 'changed', 'deprecated', 'removed', 'fixed', 'security')),
    breaking           BOOLEAN NOT NULL DEFAULT false,
    title              TEXT NOT NULL,
    body               TEXT NOT NULL DEFAULT '',
    affected_endpoints TEXT[] NOT NULL DEFAULT '{}',  -- e.g. {"/v1/api/campaigns"}
    created_by         TEXT,                          -- JWT subject of the staff author
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Public feed order: newest release first, then most-recently-authored.
CREATE INDEX idx_changelog_order ON changelog_entries (release_date DESC, created_at DESC);
CREATE INDEX idx_changelog_breaking ON changelog_entries (release_date DESC) WHERE breaking;

-- +goose Down
DROP TABLE IF EXISTS changelog_entries;
