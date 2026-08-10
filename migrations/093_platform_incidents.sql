-- +goose Up
-- Phase 11 (Business Operations) item 107: Public Status Page. Platform-wide
-- incident records shown on the public /status page and managed by staff.
-- NOT tenant-scoped (incidents are platform-global operational telemetry, like
-- batch_runs) — no account_id, no RLS. The gateway reads it directly.
CREATE TABLE incidents (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title               TEXT NOT NULL,
    body                TEXT NOT NULL DEFAULT '',
    -- impact drives the public banner colour + how it factors into overall status.
    impact              TEXT NOT NULL DEFAULT 'minor'
                          CHECK (impact IN ('none', 'minor', 'major', 'critical')),
    -- lifecycle of the incident write-up.
    status              TEXT NOT NULL DEFAULT 'investigating'
                          CHECK (status IN ('investigating', 'identified', 'monitoring', 'resolved')),
    affected_components TEXT[] NOT NULL DEFAULT '{}',  -- public component names, e.g. {'Bidding & Auctions'}
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at         TIMESTAMPTZ,
    created_by          TEXT,   -- JWT subject of the staff author (e.g. "user-<uuid>")
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_incidents_open ON incidents (started_at DESC) WHERE status <> 'resolved';
CREATE INDEX idx_incidents_recent ON incidents (started_at DESC);

-- +goose Down
DROP TABLE incidents;
