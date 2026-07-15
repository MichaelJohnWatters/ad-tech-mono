-- +goose Up

-- Audience-onboarding match rate: the fraction of a segment's uploaded ids
-- resolvable via identity_graph. THE onboarding product number — an
-- advertiser uploading a CRM list wants to know how much of it is reachable.
-- Persisted per segment on every upload (portal CSV / API JSON / drop-zone).
ALTER TABLE audience_segments ADD COLUMN match_rate DOUBLE PRECISION;
ALTER TABLE audience_segments ADD COLUMN last_upload_at TIMESTAMPTZ;

-- Drop-zone ingestion runs: one row per file the pipeline's onboarding
-- poller processed from the adtech-onboarding bucket. Feeds the staff
-- onboarding monitor (job status, rejected-row counts, per-provider match
-- rates). Rejected rows themselves persist as CSV objects in
-- {provider}/rejected/ — rejected_key points at that object.
--
-- Platform-global operational telemetry, NOT tenant data — written by the
-- pipeline service, read only by the staff monitor (account-type gated at the
-- gateway). No RLS, matching the identity_graph precedent (migration 016):
-- account_id here is an annotation of whose segment the file fed, not a
-- tenancy boundary, and it is legitimately NULL when a file fails before its
-- provider manifest resolves an account.
CREATE TABLE onboarding_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL,
    file_key TEXT NOT NULL,
    account_id UUID,
    segment_id UUID,
    status TEXT NOT NULL CHECK (status IN ('completed', 'failed')),
    total_rows INT NOT NULL DEFAULT 0,
    valid_rows INT NOT NULL DEFAULT 0,
    rejected_rows INT NOT NULL DEFAULT 0,
    matched_rows INT NOT NULL DEFAULT 0,
    match_rate DOUBLE PRECISION,
    error TEXT,
    rejected_key TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_onboarding_runs_provider ON onboarding_runs (provider, finished_at DESC);

-- +goose Down
DROP TABLE onboarding_runs;
ALTER TABLE audience_segments DROP COLUMN last_upload_at;
ALTER TABLE audience_segments DROP COLUMN match_rate;
