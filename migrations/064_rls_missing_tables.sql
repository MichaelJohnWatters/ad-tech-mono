-- +goose Up
-- +goose StatementBegin

-- RLS was missing on three tenant-scoped tables (audit finding): they carry an
-- account_id but had no row-level-security policy, unlike the ~20 tables covered
-- by migration 017. Add the same tenant_isolation pattern for consistency +
-- defense-in-depth.
--
-- NOTE: the app currently connects as a SUPERUSER (rolbypassrls), so RLS is
-- bypassed for the app itself — this policy only bites a non-superuser role. The
-- real hardening is to give the app a LIMITED role so RLS becomes an active
-- safety net; that's tracked separately (riskier: the cross-tenant warm-cache
-- loaders — BalanceLoader etc. — would then need an explicit bypass path).
-- Applying this now is safe (no behavior change under the superuser) and makes
-- the schema correct for that future downgrade.

ALTER TABLE creative_review_queue ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON creative_review_queue
    USING (account_id = current_setting('app.current_account_id')::UUID);

ALTER TABLE team_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON team_members
    USING (account_id = current_setting('app.current_account_id')::UUID);

ALTER TABLE advertiser_balances ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON advertiser_balances
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS tenant_isolation ON advertiser_balances;
ALTER TABLE advertiser_balances DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON team_members;
ALTER TABLE team_members DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON creative_review_queue;
ALTER TABLE creative_review_queue DISABLE ROW LEVEL SECURITY;
-- +goose StatementEnd
