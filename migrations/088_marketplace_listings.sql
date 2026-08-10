-- +goose Up
-- Data Marketplace slice 1: a storefront listing that publishes a data owner's
-- PUBLIC audience segment for other tenants to discover + (later slices)
-- purchase access to. The listing carries the aggregate-only shop window
-- (size, preview) + the CPM surcharge a buyer pays per impression served using
-- the segment. It NEVER exposes individual members — the segment members stay
-- with the owner; a purchase (slice 2) only grants targeting rights, settled per
-- impression through the existing data-fee pipeline (slice 3).
--
-- Builds on the shipped data-monetization foundation: audience_segments
-- (visibility public/dsp_private, data_fee_micros, taxonomy label, provider_id),
-- data_providers (ADR 0009), and the data_fee_pending → data_fee_earnings ledger.
CREATE TABLE marketplace_listings (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The seller/owner account (the segment's account).
    account_id           UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- The listed segment. One listing per segment.
    segment_id           UUID NOT NULL REFERENCES audience_segments(id) ON DELETE CASCADE,
    name                 TEXT NOT NULL,
    description          TEXT NOT NULL DEFAULT '',
    -- Aggregate reach shown in the catalog (snapshot of segment size_estimate).
    size_estimate        BIGINT NOT NULL DEFAULT 0,
    -- The CPM surcharge (micro-dollars) a buyer pays per impression served using
    -- this segment — the marketplace price. Settled to the seller minus platform
    -- margin (slice 3).
    cpm_surcharge_micros BIGINT NOT NULL DEFAULT 0,
    -- Aggregate-only shop window (top IAB categories, geo/device split, …). JSONB
    -- so the shape can grow without a migration. NEVER individual users.
    preview              JSONB NOT NULL DEFAULT '{}',
    status               TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'paused', 'withdrawn')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One listing per segment (re-listing upserts).
CREATE UNIQUE INDEX idx_marketplace_listings_segment ON marketplace_listings (segment_id);
-- The seller's own listings.
CREATE INDEX idx_marketplace_listings_account ON marketplace_listings (account_id, created_at DESC);
-- Catalog browse (active listings, newest first).
CREATE INDEX idx_marketplace_listings_status ON marketplace_listings (status, created_at DESC);

-- RLS: the seller manages their own listings (tenant_isolation); the CATALOG
-- browse is cross-tenant — any tenant sees every active listing — via the
-- platform_read hatch, same pattern as the SSP reading public segments. The
-- NULLIF guard makes the account-id cast empty-string-safe (migration 087).
ALTER TABLE marketplace_listings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON marketplace_listings
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE marketplace_listings;
