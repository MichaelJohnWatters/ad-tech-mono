-- +goose Up

-- The 90-day account purge (the last account-closure deferral): a closed account's
-- data is destructively removed once closed_at + RetentionDays has elapsed. Mark
-- the closure 'purged' (a terminal state past 'closed') + stamp purged_at so the
-- purge runs EXACTLY ONCE per account and the closure row survives as a tombstone.
ALTER TABLE account_closure_requests
    DROP CONSTRAINT IF EXISTS account_closure_requests_status_check;
ALTER TABLE account_closure_requests
    ADD CONSTRAINT account_closure_requests_status_check
    CHECK (status IN ('grace', 'cancelled', 'closed', 'purged'));

ALTER TABLE account_closure_requests
    ADD COLUMN IF NOT EXISTS purged_at TIMESTAMPTZ;

-- The purge job scans closed closures whose retention has elapsed.
CREATE INDEX IF NOT EXISTS idx_account_closure_purge_due
    ON account_closure_requests (closed_at) WHERE status = 'closed';

-- +goose Down
DROP INDEX IF EXISTS idx_account_closure_purge_due;
ALTER TABLE account_closure_requests DROP COLUMN IF EXISTS purged_at;
ALTER TABLE account_closure_requests
    DROP CONSTRAINT IF EXISTS account_closure_requests_status_check;
ALTER TABLE account_closure_requests
    ADD CONSTRAINT account_closure_requests_status_check
    CHECK (status IN ('grace', 'cancelled', 'closed'));
