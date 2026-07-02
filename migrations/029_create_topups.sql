-- +goose Up

-- Advertiser balance topups (prepay credits).
--
-- Money-touching: every topup is (1) an idempotent row here — the
-- UNIQUE (account_id, idempotency_key) is what makes client retries
-- safe — and (2) a double-entry pair in ledger_entries
-- (debit platform:cash / credit advertiser:{id}:balance), written in
-- the same transaction as the advertiser_balances upsert, so the
-- balance column is always the sum of its ledger entries.
--
-- status: the dev/fake payment path writes 'succeeded' directly. A
-- real payment provider integration would insert 'pending' and flip
-- to 'succeeded'/'failed' from the provider webhook — the ledger and
-- balance writes then move to that transition.
CREATE TABLE topups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    amount DECIMAL NOT NULL CHECK (amount > 0),
    currency TEXT NOT NULL DEFAULT 'USD',
    status TEXT NOT NULL DEFAULT 'succeeded' CHECK (status IN ('pending', 'succeeded', 'failed')),
    payment_method TEXT NOT NULL DEFAULT 'dev',
    idempotency_key TEXT NOT NULL,
    created_by UUID REFERENCES team_members(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, idempotency_key)
);

CREATE INDEX idx_topups_account ON topups (account_id);

ALTER TABLE topups ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON topups USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON topups;
DROP TABLE topups;
