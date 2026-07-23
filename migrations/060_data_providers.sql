-- +goose Up

-- data_providers — a first-class DMP entity (ADR 0009). Promotes the free-text
-- audience_ingest_jobs.provider string into a real, tenant-owned record that
-- carries a provider's data-party classification, licence, delivery format
-- expectations, encryption contract, and notification defaults.
--
-- Tenant-scoped (RLS, like audience_mappings): in this platform providers are
-- onboarded per-account (an advertiser's own CRM/CDP, or a 3rd-party feed that
-- tenant contracts). The nullable `scope` column is the seam for a future
-- PLATFORM-global catalogue (the DMP monetization layer) — not used yet.
CREATE TABLE data_providers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    -- What kind of source this is; drives the default_party default when omitted.
    kind TEXT NOT NULL DEFAULT 'other' CHECK (kind IN ('crm', 'dmp', 'cdp', 'agency', 'other')),
    -- Data-party classification stamped onto segments/signals ingested from this
    -- provider. first = our own relationship; second = a named partner's
    -- 1st-party shared under agreement; third = purchased/aggregated.
    default_party TEXT NOT NULL DEFAULT 'first' CHECK (default_party IN ('first', 'second', 'third')),
    -- Licence recorded on signals (permitted-use provenance).
    default_licence TEXT NOT NULL DEFAULT 'first_party' CHECK (default_licence IN ('first_party', 'purchased', 'barter')),
    -- id_type stamped on rows without an explicit id_type column.
    default_id_type TEXT NOT NULL DEFAULT 'user_id',
    -- Contract flag: when true, a cleartext (non-PGP) file from this provider is
    -- rejected on ingest (Phase 4). The PGP decrypt KEY stays platform-wide.
    encryption_expected BOOLEAN NOT NULL DEFAULT false,
    -- Default completion-email recipients for this provider's ingests (merged
    -- with the uploader + per-upload additional_emails at enqueue).
    notify_emails TEXT[] NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused')),
    -- Deferred DMP-monetization seams (ADR 0009 non-goals) — nullable + unused
    -- until the marketplace/rate-card layer is built.
    scope TEXT,               -- 'tenant' (implicit) | 'platform' (future global catalogue)
    cost_cpm_micros BIGINT,   -- data cost per mille, for future CPM billing
    taxonomy_vendor_id TEXT,  -- future IAB taxonomy syndication
    contract_ref TEXT,        -- future data-share contract reference
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A tenant's provider name is unique — re-saving the same name upserts.
    UNIQUE (account_id, name)
);

CREATE INDEX idx_data_providers_account ON data_providers (account_id, created_at DESC);

ALTER TABLE data_providers ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON data_providers
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- Additive, nullable provider links (providers are opt-in; plain uploads and the
-- ADR-0008 tables keep working with provider_id NULL).
ALTER TABLE audience_mappings     ADD COLUMN provider_id UUID REFERENCES data_providers(id) ON DELETE SET NULL;
ALTER TABLE audience_ingest_jobs  ADD COLUMN provider_id UUID REFERENCES data_providers(id) ON DELETE SET NULL;
ALTER TABLE audience_segments     ADD COLUMN provider_id UUID REFERENCES data_providers(id) ON DELETE SET NULL;

-- data_party is the derived party classification stamped at ingest (upload
-- override → provider default_party → 'first'). Reportable/targetable/GDPR-keyable
-- without parsing the licence string.
ALTER TABLE audience_segments
    ADD COLUMN data_party TEXT CHECK (data_party IN ('first', 'second', 'third'));

CREATE INDEX idx_segments_provider ON audience_segments (provider_id);
CREATE INDEX idx_segments_data_party ON audience_segments (data_party);

-- +goose Down
DROP INDEX IF EXISTS idx_segments_data_party;
DROP INDEX IF EXISTS idx_segments_provider;
ALTER TABLE audience_segments    DROP COLUMN IF EXISTS data_party;
ALTER TABLE audience_segments    DROP COLUMN IF EXISTS provider_id;
ALTER TABLE audience_ingest_jobs DROP COLUMN IF EXISTS provider_id;
ALTER TABLE audience_mappings    DROP COLUMN IF EXISTS provider_id;
DROP POLICY IF EXISTS tenant_isolation ON data_providers;
DROP TABLE data_providers;
