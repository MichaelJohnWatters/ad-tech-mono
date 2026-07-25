-- +goose Up
-- +goose StatementBegin

-- Platform-read escape hatch for the app-DB-role downgrade (security #77, see
-- docs/SECURITY_HARDENING.md §3). Today the app connects as a superuser so RLS
-- is bypassed; when it's downgraded to a NOBYPASSRLS role, the cross-tenant
-- warm-cache loaders (every LoadAll in pkg/store/postgres/ — CampaignLoader,
-- DealLoader, BalanceLoader, …) would filter to nothing because they run with
-- no app.current_account_id set. This migration gives every tenant_isolation
-- policy an explicit, auditable escape hatch that those loaders opt into with
-- `SET LOCAL app.platform_read = 'on'` (the QueryPlatform store helper).
--
-- Two transforms per policy, PRESERVING each policy's existing tenant logic
-- (the 33 policies have 4 distinct forms incl. subquery/child-table scoping):
--   1. make current_setting('app.current_account_id') missing_ok (add `, true`)
--      so a platform-read tx (no account_id set) matches nothing instead of
--      RAISE-ing "unrecognized configuration parameter"
--   2. OR in the explicit app.platform_read='on' flag
--
-- No-op under the current superuser (policy expressions are never evaluated
-- when RLS is bypassed), so this is safe to land ahead of the role flip.
-- Idempotent: the position() guard skips policies already carrying the hatch.
DO $$
DECLARE
    r RECORD;
    new_using text;
BEGIN
    FOR r IN
        SELECT schemaname, tablename, qual
        FROM pg_policies
        WHERE policyname = 'tenant_isolation'
    LOOP
        -- 1. missing_ok: only matches the non-missing_ok form (the `)` after
        --    ::text distinguishes it from an already-`, true)` occurrence).
        new_using := replace(
            r.qual,
            'current_setting(''app.current_account_id''::text)',
            'current_setting(''app.current_account_id''::text, true)'
        );
        -- 2. add the platform-read OR-clause unless it's already present.
        IF position('app.platform_read' in new_using) = 0 THEN
            new_using := '(' || new_using
                || ') OR (current_setting(''app.platform_read''::text, true) = ''on'')';
        END IF;
        EXECUTE format(
            'ALTER POLICY tenant_isolation ON %I.%I USING (%s)',
            r.schemaname, r.tablename, new_using
        );
    END LOOP;
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Strip the platform-read OR-clause, restoring the tenant-only USING. The
-- missing_ok `, true` is left in place (strictly safer than the strict form and
-- a no-op under the superuser); only the escape hatch is removed. Postgres
-- stores the hatch normalised as `(<inner>) OR (current_setting('app.platform
-- _read'::text, true) = 'on'::text)`, so a regexp captures <inner> reliably
-- across all policy forms (incl. multi-line subquery quals — the default flags
-- let `.` match newlines and `^`/`$` anchor the whole string).
DO $$
DECLARE
    r RECORD;
    inner_using text;
BEGIN
    FOR r IN
        SELECT schemaname, tablename, qual
        FROM pg_policies
        WHERE policyname = 'tenant_isolation'
          AND qual LIKE '%app.platform_read%'
    LOOP
        inner_using := regexp_replace(
            r.qual,
            '^\((.*) OR \(current_setting\(''app\.platform_read''::text, true\) = ''on''::text\)\)$',
            '\1'
        );
        EXECUTE format(
            'ALTER POLICY tenant_isolation ON %I.%I USING (%s)',
            r.schemaname, r.tablename, inner_using
        );
    END LOOP;
END $$;

-- +goose StatementEnd
