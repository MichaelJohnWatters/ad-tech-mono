-- +goose Up
-- Data Marketplace slice 3: CPM-surcharge settlement. When an INTERNAL buyer
-- wins an impression on a campaign that targets a segment they PURCHASED (a
-- marketplace grant), the listing's CPM surcharge settles per impression: the
-- buyer pays, the seller earns net, the platform keeps a margin. One row per
-- (trace, segment) — the PK doubles as the exactly-once claim (a redelivered
-- impression conflicts and skips the ledger). Mirrors data_fee_earnings but for
-- the internal-buyer-buys-purchased-data case (distinct from the external-buyer
-- data-fee flow; a segment can earn BOTH streams independently).
CREATE TABLE marketplace_surcharge_earnings (
    trace_id           TEXT NOT NULL,
    segment_id         TEXT NOT NULL,
    buyer_account_id   UUID NOT NULL,   -- the internal advertiser who bought the data
    seller_account_id  UUID NOT NULL,   -- the segment owner who earns
    publisher_id       TEXT NOT NULL DEFAULT '',
    placement_id       TEXT NOT NULL DEFAULT '',
    surcharge_micros   BIGINT NOT NULL, -- per-impression surcharge (listing CPM / 1000)
    seller_net_micros  BIGINT NOT NULL, -- surcharge × (1 − margin%)
    margin_micros      BIGINT NOT NULL, -- surcharge × margin%
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (trace_id, segment_id)
);

CREATE INDEX idx_marketplace_earnings_seller ON marketplace_surcharge_earnings (seller_account_id, created_at DESC);
CREATE INDEX idx_marketplace_earnings_buyer ON marketplace_surcharge_earnings (buyer_account_id, created_at DESC);

-- RLS: BOTH parties read their side (buyer spent / seller earned); reporting
-- accrues cross-tenant under the platform hatch (USING-only policy admits the
-- write). NULLIF empty-safe (mig 087).
ALTER TABLE marketplace_surcharge_earnings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON marketplace_surcharge_earnings
    USING (
        buyer_account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR seller_account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE marketplace_surcharge_earnings;
