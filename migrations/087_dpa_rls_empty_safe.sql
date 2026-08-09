-- +goose Up
-- Make the DPA tables' RLS policies empty-safe. The original policies (mig 083
-- products, 084 retargeting_product_views, 082 retargeting_suppressions) cast
-- current_setting('app.current_account_id', true)::UUID directly. Under the
-- app role that cast raises 22P02 ("invalid input syntax for type uuid: '' ")
-- when the GUC is the EMPTY STRING (a pooled connection whose prior tx set it)
-- rather than unset — even though the platform_read hatch (the OR branch) would
-- admit the row. The platform-hatch reads (ComplementSKUs, CountProductViews)
-- hit this on the conversion path. NULLIF(..,'') turns the empty string into
-- NULL so the cast yields NULL (no error) and the row is decided by the hatch.
DROP POLICY IF EXISTS tenant_isolation ON products;
CREATE POLICY tenant_isolation ON products
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

DROP POLICY IF EXISTS tenant_isolation ON retargeting_product_views;
CREATE POLICY tenant_isolation ON retargeting_product_views
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

DROP POLICY IF EXISTS tenant_isolation ON retargeting_suppressions;
CREATE POLICY tenant_isolation ON retargeting_suppressions
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    )
    WITH CHECK (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON products;
CREATE POLICY tenant_isolation ON products
    USING (account_id = current_setting('app.current_account_id', true)::UUID OR current_setting('app.platform_read', true) = 'on')
    WITH CHECK (account_id = current_setting('app.current_account_id', true)::UUID OR current_setting('app.platform_read', true) = 'on');
DROP POLICY IF EXISTS tenant_isolation ON retargeting_product_views;
CREATE POLICY tenant_isolation ON retargeting_product_views
    USING (account_id = current_setting('app.current_account_id', true)::UUID OR current_setting('app.platform_read', true) = 'on')
    WITH CHECK (account_id = current_setting('app.current_account_id', true)::UUID OR current_setting('app.platform_read', true) = 'on');
DROP POLICY IF EXISTS tenant_isolation ON retargeting_suppressions;
CREATE POLICY tenant_isolation ON retargeting_suppressions
    USING (account_id = current_setting('app.current_account_id', true)::UUID OR current_setting('app.platform_read', true) = 'on')
    WITH CHECK (account_id = current_setting('app.current_account_id', true)::UUID OR current_setting('app.platform_read', true) = 'on');
