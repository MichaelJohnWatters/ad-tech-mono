-- +goose Up
-- Phase 11 (Business Operations) item 105: Account Closure and Data Export.
-- When an account owner requests closure, a 30-day grace period starts: the
-- account is suspended, its live campaigns are paused and its placements
-- deactivated (no new spend, no new auctions), but the owner can still sign in
-- and CANCEL during the grace window. This table is the closure state machine.
--
-- To make the pause/deactivate EXACTLY reversible on cancel, the ids that this
-- closure touched are captured here (paused_line_items / deactivated_placements)
-- so cancel restores precisely those rows — never a campaign the user had paused
-- themselves before requesting closure.
CREATE TABLE account_closure_requests (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id             UUID NOT NULL REFERENCES accounts(id),
    status                 TEXT NOT NULL DEFAULT 'grace'
                             CHECK (status IN ('grace', 'cancelled', 'closed')),
    reason                 TEXT NOT NULL DEFAULT '',
    requested_by           TEXT,           -- JWT subject of the initiator (e.g. "user-<uuid>"); nullable for system-initiated
    requested_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    grace_ends_at          TIMESTAMPTZ NOT NULL,
    paused_line_items      UUID[] NOT NULL DEFAULT '{}',  -- line items this closure paused (for exact reversal)
    deactivated_placements UUID[] NOT NULL DEFAULT '{}',  -- placements this closure deactivated
    cancelled_at           TIMESTAMPTZ,
    closed_at              TIMESTAMPTZ,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one ACTIVE (grace) closure per account; a cancelled/closed one can
-- coexist as history and a fresh request can be filed later.
CREATE UNIQUE INDEX idx_account_closure_active ON account_closure_requests (account_id) WHERE status = 'grace';
CREATE INDEX idx_account_closure_account ON account_closure_requests (account_id, requested_at DESC);
-- The close-out job (slice 3) scans for grace requests whose window has elapsed.
CREATE INDEX idx_account_closure_due ON account_closure_requests (grace_ends_at) WHERE status = 'grace';

-- RLS: the account sees its own closure requests; the close-out job reads
-- cross-tenant under the platform hatch. NULLIF empty-safe (mig 087).
ALTER TABLE account_closure_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON account_closure_requests
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE account_closure_requests;
