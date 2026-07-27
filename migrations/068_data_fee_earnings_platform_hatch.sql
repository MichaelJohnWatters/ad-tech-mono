-- +goose Up
-- +goose StatementBegin

-- Security #77 gap fix: migration 065 gave every tenant_isolation policy the
-- app.platform_read hatch, but its DO-loop filtered on `policyname =
-- 'tenant_isolation'` and so SKIPPED data_fee_earnings — whose policy is named
-- `data_fee_earnings_tenant` (migration 063). Under the NOBYPASSRLS adtech_app
-- role the cross-tenant data-fee accrual (cmd/reporting/datafee.go) sets only
-- app.platform_read; the un-hatched policy still evaluated
-- current_setting('app.current_account_id') with NO missing_ok, which RAISEs
-- "unrecognized configuration parameter" (42704) when the GUC is unset and
-- silently failed every data-fee earnings insert.
--
-- Rewrite the policy to the same shape migration 065 produces: NULLIF(...,'')
-- with missing_ok on the tenant GUC (so an unset/empty value is NULL, not an
-- error) OR the explicit platform_read hatch for the cross-tenant settlement.
DROP POLICY IF EXISTS data_fee_earnings_tenant ON data_fee_earnings;
CREATE POLICY data_fee_earnings_tenant ON data_fee_earnings
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::uuid
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS data_fee_earnings_tenant ON data_fee_earnings;
CREATE POLICY data_fee_earnings_tenant ON data_fee_earnings
    USING (account_id = current_setting('app.current_account_id')::UUID);
-- +goose StatementEnd
