-- +goose Up

-- Where a publisher's earnings actually go. Payout HISTORY (the payouts table)
-- and rev-share terms already exist, but a publisher had no way to configure the
-- destination (bank/PayPal/etc) or a minimum payout threshold. This table holds
-- exactly that: one active payout method per publisher account.
--
-- ONE-ACTIVE-METHOD DECISION: enforced by a partial UNIQUE index on account_id
-- WHERE status='active'. The gateway upsert only ever writes status='active' and
-- ON CONFLICT-updates the existing active row, so in practice there is a single
-- row per account; the index is the DB-level guarantee against a second active
-- method sneaking in.
--
-- SENSITIVE MATERIAL: the raw destination fields (account_number, iban,
-- paypal_email, …) live in `details` (JSONB). The API never returns them — reads
-- return only the cached `last4` masked tail. Treat `details` like credential
-- material: written, never read back through the API.
CREATE TABLE payout_methods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- method_type buckets the destination: bank_transfer | paypal | payoneer |
    -- wire. Validated in the gateway (required detail fields differ per type).
    method_type TEXT NOT NULL,
    display_name TEXT,
    -- Raw sensitive destination fields (account_number/iban/paypal_email/…).
    -- Never returned through the API — only `last4` is exposed on read.
    details JSONB NOT NULL DEFAULT '{}',
    -- Cached masked tail (e.g. "6789" or "j***@example.com") so list/read paths
    -- render a recognisable hint without decrypting/parsing `details`.
    last4 TEXT,
    minimum_payout_cents BIGINT NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT 'USD',
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One active payout method per publisher account (see decision note above).
CREATE UNIQUE INDEX idx_payout_methods_account_active
    ON payout_methods (account_id) WHERE status = 'active';

ALTER TABLE payout_methods ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payout_methods
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON payout_methods;
DROP TABLE payout_methods;
