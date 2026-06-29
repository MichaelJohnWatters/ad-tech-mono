-- +goose Up
-- Publisher-side direct-sold line items. Distinct from line_items (which is
-- the advertiser-side line item — what the DSP bids on). These rows are the
-- publisher's own commitments: "I sold Nike 1M sponsorship impressions on
-- the homepage this month for $50 CPM."
--
-- The publisher-adserver service consumes these on every ad request and
-- arbitrates them against programmatic (the SSP→exchange→DSP chain). See
-- docs/PLAN.md → "Publisher-Side Ad Server" for the full arbitration ladder.
--
-- account_id is the publisher's account (not the buyer's — direct deals
-- often involve brands that never log into our DSP, recorded only as text
-- in demand_source). RLS still applies because the publisher owns the row.
--
-- impressions_committed = 0 means no delivery obligation (house ads,
-- non-guaranteed preferred line items). Non-zero rows are subject to pacing
-- by the publisher-adserver's pacing controller.
--
-- placement_ids = '{}' means "any placement under this publisher_id". The
-- arbitration code filters publisher_id first, then placement_ids if the
-- array is non-empty.

CREATE TABLE publisher_line_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    publisher_id UUID NOT NULL REFERENCES publishers(id),
    name TEXT NOT NULL,
    demand_source TEXT NOT NULL DEFAULT '',
    priority_tier TEXT NOT NULL CHECK (priority_tier IN (
        'sponsorship', 'guaranteed', 'preferred', 'house'
    )),
    placement_ids UUID[] NOT NULL DEFAULT '{}',
    impressions_committed BIGINT NOT NULL DEFAULT 0,
    delivery_start TIMESTAMPTZ,
    delivery_end TIMESTAMPTZ,
    cpm DECIMAL,
    currency TEXT NOT NULL DEFAULT 'USD',
    pacing_mode TEXT NOT NULL DEFAULT 'even' CHECK (pacing_mode IN ('even', 'asap')),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN (
        'draft', 'active', 'paused', 'ended'
    )),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_pli_publisher ON publisher_line_items (publisher_id);
CREATE INDEX idx_pli_account ON publisher_line_items (account_id);
CREATE INDEX idx_pli_status_tier ON publisher_line_items (status, priority_tier);

CREATE TABLE publisher_line_item_creatives (
    publisher_line_item_id UUID NOT NULL REFERENCES publisher_line_items(id) ON DELETE CASCADE,
    creative_id UUID NOT NULL REFERENCES creatives(id),
    weight INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (publisher_line_item_id, creative_id)
);

CREATE INDEX idx_plic_creative ON publisher_line_item_creatives (creative_id);

ALTER TABLE publisher_line_items ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON publisher_line_items
    USING (account_id = current_setting('app.current_account_id')::UUID);

ALTER TABLE publisher_line_item_creatives ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON publisher_line_item_creatives USING (
    publisher_line_item_id IN (
        SELECT id FROM publisher_line_items
        WHERE account_id = current_setting('app.current_account_id')::UUID
    )
);

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON publisher_line_item_creatives;
ALTER TABLE publisher_line_item_creatives DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON publisher_line_items;
ALTER TABLE publisher_line_items DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS publisher_line_item_creatives;
DROP TABLE IF EXISTS publisher_line_items;
