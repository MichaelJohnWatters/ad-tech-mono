-- +goose Up

-- Per-account durable in-app notifications. Budget/balance-depleted and
-- campaign-state-changed business events already fire on NATS (consumed by the
-- webhooks dispatcher for HTTP delivery), but there was no per-account record a
-- signed-in user could see in the portal. cmd/notifications consumes those same
-- subjects and writes one row per account here; the gateway serves the bell +
-- dropdown (list / unread-count / mark-read) from it. account_id is the tenant
-- owner (the advertiser/publisher whose portal shows the bell).
CREATE TABLE notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- kind buckets the notification for icon/colour in the UI and is a stable
    -- programmatic key (budget_depleted | balance_depleted | campaign_state |
    -- report_ready | …). Free text (not a CHECK) so a new event → new kind is a
    -- one-line consumer change, no migration.
    kind TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT,
    -- ref_id points at the subject entity (campaign id, report job id) so the
    -- UI can deep-link; nullable because account-level events (balance) have none.
    ref_id TEXT,
    read BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The bell's two queries — unread count and the recent-first list — both filter
-- by account_id and order by created_at desc; the unread-count query also
-- filters read. One composite index serves both.
CREATE INDEX idx_notifications_account_read_created
    ON notifications (account_id, read, created_at DESC);

ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notifications
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON notifications;
DROP TABLE notifications;
