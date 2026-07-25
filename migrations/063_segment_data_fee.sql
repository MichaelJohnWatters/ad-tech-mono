-- +goose Up
-- Data monetization (the ADR 0009 deferred seam, phase 1): a PUBLIC segment
-- may carry a data fee. When an EXTERNAL bidder wins an auction whose bid
-- request carried the segment (as consent-gated user.data — see migration
-- 062), the segment's owning account earns the fee per DELIVERED impression,
-- net of the platform margin (billing.data_fee_margin_pct).
--
-- Units: CPM in MICRO-dollars, the platform money convention — 500000 =
-- $0.50 CPM → $0.0005 per impression. NULL = not monetized (the default;
-- nothing changes for existing segments). Fees are only meaningful on
-- visibility='public' rows: dsp_private segments never ride bid requests.
ALTER TABLE audience_segments
    ADD COLUMN data_fee_micros BIGINT;

-- The SSP's warm monetization map filters on (public, fee-bearing).
CREATE INDEX idx_segments_data_fee ON audience_segments (data_fee_micros)
    WHERE data_fee_micros IS NOT NULL;

-- The reporting-side durable join: a DataFeeEvent (published by the SSP at
-- auction time) waits here until the impression for its trace arrives, then
-- accrues and is deleted. Durable because reporting replicas PARTITION the
-- event stream — the fee event and the impression may land on different
-- pods, so an in-memory join would silently drop fees at >1 replica.
-- Service-internal cross-tenant table (like batch_runs): no account_id, no
-- RLS — rows exist only between win and impression (or expiry sweep).
CREATE TABLE data_fee_pending (
    trace_id   TEXT PRIMARY KEY,
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_data_fee_pending_created ON data_fee_pending (created_at);

-- Accrued data-fee earnings: one row per (delivered impression, fee-bearing
-- segment). account_id is the SEGMENT OWNER — the earning tenant — so the
-- standard RLS shape applies and the portal earnings API scopes normally.
-- Amounts are per-impression MICRO-dollars: fee_micros = the full fee
-- (CPM/1000), owner_net_micros + margin_micros = its split. The winner_seat
-- is the external buyer that owes the fee (receivable; invoiced out-of-band
-- — external seats have no prepay balance).
CREATE TABLE data_fee_earnings (
    trace_id         TEXT NOT NULL,
    segment_id       TEXT NOT NULL,
    account_id       UUID NOT NULL,
    winner_seat      TEXT NOT NULL,
    publisher_id     TEXT NOT NULL DEFAULT '',
    placement_id     TEXT NOT NULL DEFAULT '',
    fee_micros       BIGINT NOT NULL,
    owner_net_micros BIGINT NOT NULL,
    margin_micros    BIGINT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (trace_id, segment_id)
);

CREATE INDEX idx_data_fee_earnings_account ON data_fee_earnings (account_id, created_at);

ALTER TABLE data_fee_earnings ENABLE ROW LEVEL SECURITY;
CREATE POLICY data_fee_earnings_tenant ON data_fee_earnings
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose Down
DROP TABLE data_fee_earnings;
DROP TABLE data_fee_pending;
DROP INDEX IF EXISTS idx_segments_data_fee;
ALTER TABLE audience_segments DROP COLUMN IF EXISTS data_fee_micros;
