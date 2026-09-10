-- +goose Up

-- The payout runner (cmd/payout-runner) generates ONE payout per publisher per
-- period, and must be safely re-runnable (a monthly CronJob that may retry, plus
-- the account-closeout final-payout path). Mirror the invoice runner's
-- idempotency guard (migration 051's uq_invoices_account_period): a UNIQUE key on
-- (publisher_id, period_start, period_end) so a re-run UPSERTs the same row
-- instead of writing a duplicate. The runner's ON CONFLICT updates amount/
-- platform_fee ONLY while status='pending' (an already 'paid'/'processing' payout
-- is never rewritten).
ALTER TABLE payouts
    ADD CONSTRAINT uq_payouts_publisher_period
    UNIQUE (publisher_id, period_start, period_end);

-- +goose Down
ALTER TABLE payouts DROP CONSTRAINT IF EXISTS uq_payouts_publisher_period;
