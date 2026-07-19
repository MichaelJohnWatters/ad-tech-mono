-- +goose Up

-- Per-campaign breakdown of an advertiser invoice. The invoices table (migration
-- 012) already carries the header (account, period, total, status, due date) but
-- there was no line-level detail and nothing generated invoices from real spend.
-- pkg/invoicing sums campaign_committed_spend.settled_micros per campaign over an
-- invoice period, converts MICROS → dollars (dollars = micros / 1_000_000.0), and
-- writes ONE invoices row + one invoice_line_items row per campaign.
--
-- account_id is denormalised from the parent invoice so RLS can filter this table
-- directly (same tenant_isolation form as every other tenant table) without a
-- join back to invoices.
CREATE TABLE invoice_line_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id UUID NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    -- Denormalised tenant owner (same account as the parent invoice) so the RLS
    -- policy filters this table on its own column, no invoices join.
    account_id UUID NOT NULL,
    -- campaign_id == line_items.id everywhere in this codebase; TEXT to match
    -- campaign_committed_spend.campaign_id (that table is FK-free by design).
    campaign_id TEXT NOT NULL,
    campaign_name TEXT,
    -- Delivery counts are populated only when cheaply available; committed spend
    -- carries no counts, so these default 0 (spend is the source of truth).
    impressions BIGINT NOT NULL DEFAULT 0,
    clicks BIGINT NOT NULL DEFAULT 0,
    conversions BIGINT NOT NULL DEFAULT 0,
    -- Dollars (DECIMAL) to match invoices.total; already converted from micros.
    spend DECIMAL NOT NULL,
    bid_model TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The detail read fetches every line for one invoice; index the FK.
CREATE INDEX idx_invoice_line_items_invoice ON invoice_line_items (invoice_id);

ALTER TABLE invoice_line_items ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoice_line_items
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- One invoice per account per period makes generation idempotent: the generator
-- ON CONFLICT-updates the header total and replaces the line items on re-run,
-- so a re-run for the same period never duplicates. Nothing seeds two invoices
-- for the same (account, period) — cmd/gateway/reset.go only TRUNCATEs invoices,
-- and no code path INSERTs them — so this constraint cannot clash with fixtures.
ALTER TABLE invoices
    ADD CONSTRAINT uq_invoices_account_period UNIQUE (account_id, period_start, period_end);

-- +goose Down
ALTER TABLE invoices DROP CONSTRAINT IF EXISTS uq_invoices_account_period;
DROP POLICY IF EXISTS tenant_isolation ON invoice_line_items;
DROP TABLE invoice_line_items;
