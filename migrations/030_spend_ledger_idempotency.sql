-- +goose Up

-- Spend-drawdown idempotency (money loop). The billing sink debits
-- advertiser_balances once per realized spend event, writing a double-entry
-- ledger pair keyed by (reference_type='spend', reference_id=trace:event).
-- NATS redelivers at-least-once, so the pair insert must be a no-op on
-- replay — this partial unique index is what ON CONFLICT DO NOTHING
-- resolves against. Scoped to 'spend' so existing reference types
-- (topup, impression, ...) keep their current multi-row semantics.
CREATE UNIQUE INDEX idx_ledger_spend_idem
    ON ledger_entries (reference_type, reference_id, account_code)
    WHERE reference_type = 'spend';

-- +goose Down
DROP INDEX idx_ledger_spend_idem;
