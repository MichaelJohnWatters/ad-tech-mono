-- +goose Up
-- Retargeting suppression memory ("burn list") — the durable half of
-- purchase suppression. Before this table, a purchase only DELETED the
-- converting id's membership rows: the hourly profile-builder re-qualified
-- the buyer from still-live site_visit signals on its next pass (and
-- cluster-expanded the enrollment to every linked id), so the buyer's other
-- devices resumed being chased within the hour (found live 2026-08-09).
--
-- One row per (advertiser account, user id) records WHEN the purchase
-- happened. Enrollment paths consult it with newer-than semantics:
--   - a signal/visit OLDER than suppressed_at never (re-)enrolls the id —
--     the purchase closed that cart;
--   - a visit NEWER than suppressed_at is a genuinely NEW abandoned cart:
--     audience-rt deletes the row (self-clearing) and enrolls normally.
-- audience-rt writes rows for the converting id PLUS its identity-cluster
-- siblings and household ids, so suppression is PERSON+HOUSEHOLD level,
-- matching the person-level enrollment the profile-builder does.
--
-- expires_at bounds table growth: past the advertiser's rule windows the
-- historical signals can no longer qualify anyone, so the row is inert.
-- The audience-rt purge ticker deletes expired rows.
CREATE TABLE retargeting_suppressions (
    account_id    UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL,
    suppressed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    source        TEXT NOT NULL DEFAULT 'purchase',
    origin_trace  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (account_id, user_id)
);

CREATE INDEX idx_retargeting_suppressions_expires ON retargeting_suppressions (expires_at);

-- RLS: audience-rt writes under the advertiser's account (tenant_isolation);
-- the profile-builder reads cross-tenant via the platform_read hatch, same
-- pattern as audience_membership_changelog / audience_segment_members.
ALTER TABLE retargeting_suppressions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON retargeting_suppressions
    USING (
        account_id = current_setting('app.current_account_id', true)::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        account_id = current_setting('app.current_account_id', true)::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE retargeting_suppressions;
