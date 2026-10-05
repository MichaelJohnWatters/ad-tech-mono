-- +goose Up
-- OpenRTB burl (billing notice, 2.5+): the buyer's billing-notice URL rides
-- the AuctionWinEvent (auction macros already substituted by the exchange)
-- and must fire at the BILLABLE moment — when the impression books in the
-- billing engine. Reporting replicas partition the event stream, so the win
-- and its impression routinely land on different pods: the join is durable
-- (same pattern as data_fee_pending). DELETE ... RETURNING claims the row
-- atomically → the notice fires exactly once even across redeliveries.
--
-- PLATFORM-GLOBAL table, deliberately NO RLS (precedent: data_fee_pending,
-- batch_runs): rows are transient seller-side plumbing keyed by trace, carry
-- no tenant-owned data, and are written/claimed by the reporting service
-- before any tenant context exists.
CREATE TABLE burl_pending (
    trace_id    TEXT PRIMARY KEY,
    billing_url TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_burl_pending_created ON burl_pending (created_at);

-- +goose Down
DROP TABLE burl_pending;
