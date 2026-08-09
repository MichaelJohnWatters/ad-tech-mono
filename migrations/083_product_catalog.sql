-- +goose Up
-- Dynamic Product Ads slice 1: the advertiser-scoped product catalog.
--
-- One row per (advertiser account, SKU). The feed arrives through the SAME
-- unified ingestion path as audience files (ADR 0007/0008): staged to object
-- storage, one audience_ingest_jobs row (kind='product'), drained by the same
-- worker / inline path — a second feed TYPE, not new machinery. Later slices
-- read this table at render time (dynamic creative assembly) and key the
-- purchase burn list by SKU, so the row carries everything a template needs:
-- title/image/price/availability/product_url.
--
-- price_micros: micro-dollars (platform money convention — see PLAN.md money
-- precision), parsed at ingest from decimal prices ("38.99" or "38.99 USD").
CREATE TABLE products (
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    sku          TEXT NOT NULL,
    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    image_url    TEXT NOT NULL DEFAULT '',
    price_micros BIGINT NOT NULL DEFAULT 0,
    currency     TEXT NOT NULL DEFAULT 'USD',
    availability TEXT NOT NULL DEFAULT 'in_stock'
        CHECK (availability IN ('in_stock', 'out_of_stock', 'preorder', 'discontinued')),
    product_url  TEXT NOT NULL DEFAULT '',
    category     TEXT NOT NULL DEFAULT '',
    -- Lineage, mirroring audience_segment_members (migration 080): which path
    -- wrote the row (api|dropzone) and the ingest job's ing_ id.
    source       TEXT NOT NULL DEFAULT 'api',
    origin_trace TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, sku)
);

-- RLS: gateway reads/writes under the advertiser tenant; the ad server's
-- render-time catalog lookup (slice 3) reads cross-tenant via the platform
-- hatch — same pattern as retargeting_suppressions (migration 082).
ALTER TABLE products ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON products
    USING (
        account_id = current_setting('app.current_account_id', true)::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        account_id = current_setting('app.current_account_id', true)::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- The unified ingest queue gains a feed KIND so one queue + one worker drain
-- both audience files and product feeds (the job row stays self-describing).
ALTER TABLE audience_ingest_jobs
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'audience'
        CHECK (kind IN ('audience', 'product'));

-- +goose Down
ALTER TABLE audience_ingest_jobs DROP COLUMN kind;
DROP TABLE products;
