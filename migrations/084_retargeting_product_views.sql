-- +goose Up
-- Dynamic Product Ads slice 2: SKU-aware retargeting memory.
--
-- The retargeting pixel (/v1/t/rt) can now carry the SKUs a shopper viewed or
-- carted (sku= / skus= CSV). audience-rt records them here — one row per
-- (advertiser account, user id, sku) — with the SAME newer-than / TTL semantics
-- as segment membership. Slice 3 (dynamic creative) reads a user's recent SKUs
-- at render time to assemble the ad from the catalog; slice 4 keys per-product
-- suppression on the bought SKU.
--
-- Written PERSON+HOUSEHOLD (the visitor id plus, when household enrollment is
-- on, the hh: id) so a co-viewer / other device sees the same carted products,
-- matching the granularity of the enrollment it rides alongside.
CREATE TABLE retargeting_product_views (
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL,
    sku          TEXT NOT NULL,
    seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    origin_trace TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (account_id, user_id, sku)
);

-- Render-time read: this user's most-recent SKUs, freshest first.
CREATE INDEX idx_rpv_user ON retargeting_product_views (account_id, user_id, seen_at DESC);
-- Purge sweep.
CREATE INDEX idx_rpv_expires ON retargeting_product_views (expires_at);

-- RLS: audience-rt writes under the advertiser account (tenant_isolation); the
-- ad server's render-time SKU lookup (slice 3) reads cross-tenant via the
-- platform_read hatch — same pattern as retargeting_suppressions (mig 082).
ALTER TABLE retargeting_product_views ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON retargeting_product_views
    USING (
        account_id = current_setting('app.current_account_id', true)::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        account_id = current_setting('app.current_account_id', true)::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE retargeting_product_views;
