-- +goose Up
-- Phase 11 (Business Operations) item 112: External Partner Onboarding.
-- A registry of external integration partners (demand DSPs, supply SSPs) that
-- integrate over OpenRTB. Today adding an external DSP is a manual edit of the
-- exchange.dsp_endpoints config string with no metadata, audit trail, or
-- lifecycle. This table makes a partner a first-class record with the onboarding
-- lifecycle: pending -> sandbox -> certified -> active (-> paused/terminated).
--
-- Platform-global operational registry (staff-managed), like incidents /
-- batch_runs — no account_id, no RLS in this slice. A later slice links a
-- partner to a self-serve `partner` account (for the partner-facing portal) and
-- adds the tenant policy then; until then only staff (via the platform hatch +
-- the partner:read/partner:manage permissions) touch it.
CREATE TABLE partners (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    -- kind of integration partner. 'dsp' = demand (bids into our exchange),
    -- 'ssp' = supply (sends us bid requests).
    kind            TEXT NOT NULL DEFAULT 'dsp' CHECK (kind IN ('dsp', 'ssp')),
    -- onboarding lifecycle. register (pending) -> sandbox (test traffic, fake
    -- money) -> certified (passed the acceptance suite) -> active (live);
    -- paused/terminated are the off-ramps.
    status          TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'sandbox', 'certified', 'active', 'paused', 'terminated')),
    endpoint_bid    TEXT NOT NULL DEFAULT '',   -- their OpenRTB bid URL (http:// or grpc://)
    endpoint_nurl   TEXT NOT NULL DEFAULT '',   -- win/loss notify base, if different from bid
    seat            TEXT NOT NULL DEFAULT '',   -- the trusted seat we bill (exchange ;seat=)
    auth_method     TEXT NOT NULL DEFAULT 'api_key'
                      CHECK (auth_method IN ('api_key', 'mtls', 'none')),
    -- ref to the secrets table (never the secret itself); wired in a later slice.
    auth_secret_ref TEXT NOT NULL DEFAULT '',
    channels        TEXT[] NOT NULL DEFAULT '{}',   -- display/video/native/audio
    formats         TEXT[] NOT NULL DEFAULT '{}',   -- banner/vast/mraid/...
    timeout_ms      INT NOT NULL DEFAULT 100,       -- our per-partner bid deadline
    max_qps         INT,                            -- optional rate cap
    openrtb_version TEXT NOT NULL DEFAULT '2.5',
    contact_tech    TEXT NOT NULL DEFAULT '',       -- engineering contact
    contact_billing TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',       -- staff notes
    created_by      TEXT,                           -- staff JWT subject who registered it
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    onboarded_at    TIMESTAMPTZ                     -- set when it first goes active
);

CREATE INDEX idx_partners_status ON partners (status, created_at DESC);
CREATE UNIQUE INDEX idx_partners_name ON partners (lower(name));

-- +goose Down
DROP TABLE partners;
