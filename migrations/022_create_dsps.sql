-- +goose Up
-- DSPs as first-class entities. Previously identity was a stack of (YAML
-- profile + pod name + config row) which had to stay in sync across three
-- places. This table makes a DSP a real row: one source of truth for the
-- name, the behavior knobs (noise, no_bid_rate), the optional endpoint
-- (for external partners), and the link from advertisers to their managing
-- DSP via accounts.dsp_id.
--
-- profile_type semantics:
--   'internal'   — bids deterministically; no noise; used for our own demand
--   'competitor' — adds noise + random no-bid to simulate other agencies
--   'external'   — real partner DSP reached via the endpoint column
--                  (covered by the External DSP Partners build, not this migration)

CREATE TABLE dsps (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL,
    profile_type  TEXT NOT NULL CHECK (profile_type IN ('internal', 'competitor', 'external')),
    noise_pct     INT NOT NULL DEFAULT 0,
    no_bid_rate   DOUBLE PRECISION NOT NULL DEFAULT 0,
    endpoint      TEXT,
    status        TEXT NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_dsps_name ON dsps (name);

-- Advertiser accounts belong to a managing DSP. NULL for non-advertiser
-- accounts (publishers, admins, staff). The CampaignLoader joins through
-- this to filter campaigns to a specific DSP's portfolio.
ALTER TABLE accounts ADD COLUMN dsp_id UUID REFERENCES dsps(id);
CREATE INDEX idx_accounts_dsp ON accounts (dsp_id) WHERE dsp_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_accounts_dsp;
ALTER TABLE accounts DROP COLUMN dsp_id;
DROP INDEX IF EXISTS idx_dsps_name;
DROP TABLE dsps;
