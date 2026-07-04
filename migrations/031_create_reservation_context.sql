-- +goose Up

-- Reservation context — settle-enrichment cache for reserve/settle bid
-- models (CPC/CPA/vCPM/CPCV).
--
-- The billing engine reserves on impression and settles on the trigger
-- event (click/conversion/viewable/complete), recovering the original
-- auction context (publisher, advertiser, campaign, deal) from the
-- reservation. The MemoryLedger keeps that context in-process, but the
-- TigerBeetle ledger stores only numeric account IDs + amounts + trace/
-- bid_model in user_data — it physically cannot round-trip the UUID
-- STRINGS. Without them, SettleByTrace built a settle event with empty
-- publisher/advertiser, TB's record failed ("invalid UUID length: 0"),
-- and the prepay drawdown no-op'd. This table is where reporting persists
-- that context on reserve so settle is ledger-backend-agnostic.
--
-- Billing-internal (like ledger_entries / budget_reservations): written and
-- read by the reporting service by trace_id only, never per-tenant, so no
-- account_id / RLS. No FK to line_items — a settle must not fail because a
-- campaign was archived between reserve and the trigger event.
CREATE TABLE reservation_context (
    trace_id      TEXT PRIMARY KEY,
    campaign_id   TEXT,
    creative_id   TEXT,
    placement_id  TEXT,
    publisher_id  TEXT,
    advertiser_id TEXT,
    deal_type     TEXT,
    currency      TEXT NOT NULL DEFAULT 'USD',
    amount        DECIMAL NOT NULL,
    bid_model     TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE reservation_context;
