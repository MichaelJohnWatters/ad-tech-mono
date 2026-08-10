-- +goose Up
-- Phase 11 (Business Operations) item 108: Customer Support and Dispute
-- Resolution. Customers raise support tickets (general questions, technical
-- issues, or billing disputes with a disputed amount); staff triage, reply, and
-- resolve — a billing dispute can resolve with a credit adjustment. A ticket has
-- a message thread (customer + staff turns).
CREATE TABLE support_tickets (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id             UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind                   TEXT NOT NULL DEFAULT 'question'
                             CHECK (kind IN ('question', 'technical', 'billing_dispute')),
    subject                TEXT NOT NULL,
    status                 TEXT NOT NULL DEFAULT 'open'
                             CHECK (status IN ('open', 'pending', 'resolved', 'closed')),
    amount_disputed_micros BIGINT,          -- billing_dispute only
    currency               TEXT NOT NULL DEFAULT 'USD',
    resolution             TEXT,            -- staff's resolution note
    assigned_to            TEXT,            -- staff JWT subject
    created_by             TEXT,            -- customer JWT subject
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at            TIMESTAMPTZ
);

CREATE INDEX idx_support_tickets_account ON support_tickets (account_id, created_at DESC);
CREATE INDEX idx_support_tickets_open ON support_tickets (created_at DESC) WHERE status IN ('open', 'pending');

-- The message thread. One row per turn; author_type keeps the customer/staff
-- distinction without leaking staff identity to customers.
CREATE TABLE support_messages (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id   UUID NOT NULL REFERENCES support_tickets(id) ON DELETE CASCADE,
    author_type TEXT NOT NULL CHECK (author_type IN ('customer', 'staff')),
    author_id   TEXT,            -- JWT subject of the author
    body        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_support_messages_ticket ON support_messages (ticket_id, created_at);

-- RLS: the owning account sees its tickets; staff read/resolve cross-tenant via
-- the platform hatch. NULLIF empty-safe (mig 087). Messages inherit visibility
-- through their ticket (the subquery is itself RLS-filtered).
ALTER TABLE support_tickets ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON support_tickets
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

ALTER TABLE support_messages ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON support_messages
    USING (ticket_id IN (SELECT id FROM support_tickets));

-- +goose Down
DROP TABLE support_messages;
DROP TABLE support_tickets;
