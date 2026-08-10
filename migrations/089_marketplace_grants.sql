-- +goose Up
-- Data Marketplace slice 2: a purchase grant. Buying access to a listing writes
-- one row here — the buyer's right to target the seller's segment. Because
-- listable segments are PUBLIC (slice 1), they already ride every bid request as
-- user.ext.segments, so bid-time matching needs NO change: the grant is the
-- AUTHORIZATION + the settlement key (slice 3 bills the CPM surcharge per
-- impression the buyer wins using the segment). The segment_id + surcharge are
-- denormalized from the listing so the settlement lookup is a single indexed
-- read, and the price the buyer agreed to is frozen even if the listing's price
-- later changes.
CREATE TABLE marketplace_grants (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id           UUID NOT NULL REFERENCES marketplace_listings(id) ON DELETE CASCADE,
    segment_id           UUID NOT NULL,                                   -- denormalized (settlement/targeting lookup)
    seller_account_id    UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    buyer_account_id     UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    cpm_surcharge_micros BIGINT NOT NULL DEFAULT 0,                       -- price snapshot at purchase
    status               TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'expired', 'cancelled')),
    granted_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at           TIMESTAMPTZ,                                     -- NULL = perpetual access
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One grant per (buyer, listing) — re-purchase renews it.
CREATE UNIQUE INDEX idx_marketplace_grants_buyer_listing ON marketplace_grants (buyer_account_id, listing_id);
-- The buyer's purchased data.
CREATE INDEX idx_marketplace_grants_buyer ON marketplace_grants (buyer_account_id, granted_at DESC);
-- The seller's sales.
CREATE INDEX idx_marketplace_grants_seller ON marketplace_grants (seller_account_id, granted_at DESC);
-- Settlement / authorization lookup: is (buyer, segment) an active grant?
CREATE INDEX idx_marketplace_grants_active ON marketplace_grants (buyer_account_id, segment_id) WHERE status = 'active';

-- RLS: a grant is a two-party record — BOTH the buyer and the seller may read
-- their side; the buyer creates it. Settlement reads cross-tenant via the
-- platform hatch. NULLIF makes the account-id cast empty-string-safe (mig 087).
ALTER TABLE marketplace_grants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON marketplace_grants
    USING (
        buyer_account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR seller_account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        buyer_account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE marketplace_grants;
