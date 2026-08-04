-- +goose Up
-- audience_membership_changelog is the outbox that drives the audience cache's
-- append-based refresh. Every membership write (retargeting enroll/suppress,
-- upload, profile-builder add/prune) appends one row here in the same unit of
-- work. A single writer polls rows past its watermark and applies them to Redis
-- as atomic SADD/SREM (no per-user rebuild, no per-pod fan-out), then trims the
-- consumed rows. seq is the monotonic watermark cursor.
CREATE TABLE audience_membership_changelog (
    seq         BIGSERIAL PRIMARY KEY,
    account_id  UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL,
    segment_id  UUID NOT NULL,
    visibility  TEXT NOT NULL,               -- public | dsp_private
    op          TEXT NOT NULL,               -- add | remove
    changed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The poller reads WHERE seq > watermark ORDER BY seq (PK already covers this).
-- An index on changed_at supports a time-based safety trim.
CREATE INDEX idx_membership_changelog_changed ON audience_membership_changelog (changed_at);

-- RLS: writers append under their own account (tenant_isolation); the single
-- cache-writer poller reads + trims cross-tenant via the platform_read hatch,
-- exactly like the preloader's cross-tenant membership scan (security #77).
ALTER TABLE audience_membership_changelog ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audience_membership_changelog
    USING (
        account_id = current_setting('app.current_account_id', true)::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        account_id = current_setting('app.current_account_id', true)::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE IF EXISTS audience_membership_changelog;
