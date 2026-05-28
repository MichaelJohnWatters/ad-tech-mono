-- +goose Up
-- Row-Level Security: defence in depth for multi-tenancy.
-- Even if application code forgets WHERE account_id = ?, Postgres blocks access.

ALTER TABLE insertion_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE line_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE creatives ENABLE ROW LEVEL SECURITY;
ALTER TABLE line_item_creatives ENABLE ROW LEVEL SECURITY;
ALTER TABLE targeting_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE publishers ENABLE ROW LEVEL SECURITY;
ALTER TABLE placements ENABLE ROW LEVEL SECURITY;
ALTER TABLE deals ENABLE ROW LEVEL SECURITY;
ALTER TABLE audience_segments ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE payouts ENABLE ROW LEVEL SECURITY;
ALTER TABLE adjustments ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE quality_controls ENABLE ROW LEVEL SECURITY;
ALTER TABLE saved_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;

-- Policy: rows visible only when app.current_account_id matches
-- Services SET LOCAL app.current_account_id = '{uuid}' before queries

CREATE POLICY tenant_isolation ON insertion_orders USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON line_items USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON creatives USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON targeting_rules USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON publishers USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON placements USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON deals USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON audience_segments USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON invoices USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON payouts USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON adjustments USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON webhooks USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON notification_preferences USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON quality_controls USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON saved_reports USING (account_id = current_setting('app.current_account_id')::UUID);
CREATE POLICY tenant_isolation ON api_keys USING (account_id = current_setting('app.current_account_id')::UUID);

-- line_item_creatives joins through line_items (inherits via line_item_id)
CREATE POLICY tenant_isolation ON line_item_creatives USING (
    line_item_id IN (SELECT id FROM line_items WHERE account_id = current_setting('app.current_account_id')::UUID)
);

-- +goose Down
-- Drop all policies and disable RLS (reverse order)
DROP POLICY IF EXISTS tenant_isolation ON line_item_creatives;
DROP POLICY IF EXISTS tenant_isolation ON api_keys;
DROP POLICY IF EXISTS tenant_isolation ON saved_reports;
DROP POLICY IF EXISTS tenant_isolation ON quality_controls;
DROP POLICY IF EXISTS tenant_isolation ON notification_preferences;
DROP POLICY IF EXISTS tenant_isolation ON webhooks;
DROP POLICY IF EXISTS tenant_isolation ON adjustments;
DROP POLICY IF EXISTS tenant_isolation ON payouts;
DROP POLICY IF EXISTS tenant_isolation ON invoices;
DROP POLICY IF EXISTS tenant_isolation ON audience_segments;
DROP POLICY IF EXISTS tenant_isolation ON deals;
DROP POLICY IF EXISTS tenant_isolation ON placements;
DROP POLICY IF EXISTS tenant_isolation ON publishers;
DROP POLICY IF EXISTS tenant_isolation ON targeting_rules;
DROP POLICY IF EXISTS tenant_isolation ON creatives;
DROP POLICY IF EXISTS tenant_isolation ON line_item_creatives;
DROP POLICY IF EXISTS tenant_isolation ON line_items;
DROP POLICY IF EXISTS tenant_isolation ON insertion_orders;

ALTER TABLE api_keys DISABLE ROW LEVEL SECURITY;
ALTER TABLE saved_reports DISABLE ROW LEVEL SECURITY;
ALTER TABLE quality_controls DISABLE ROW LEVEL SECURITY;
ALTER TABLE notification_preferences DISABLE ROW LEVEL SECURITY;
ALTER TABLE webhooks DISABLE ROW LEVEL SECURITY;
ALTER TABLE adjustments DISABLE ROW LEVEL SECURITY;
ALTER TABLE payouts DISABLE ROW LEVEL SECURITY;
ALTER TABLE invoices DISABLE ROW LEVEL SECURITY;
ALTER TABLE audience_segments DISABLE ROW LEVEL SECURITY;
ALTER TABLE deals DISABLE ROW LEVEL SECURITY;
ALTER TABLE placements DISABLE ROW LEVEL SECURITY;
ALTER TABLE publishers DISABLE ROW LEVEL SECURITY;
ALTER TABLE targeting_rules DISABLE ROW LEVEL SECURITY;
ALTER TABLE line_item_creatives DISABLE ROW LEVEL SECURITY;
ALTER TABLE creatives DISABLE ROW LEVEL SECURITY;
ALTER TABLE line_items DISABLE ROW LEVEL SECURITY;
ALTER TABLE insertion_orders DISABLE ROW LEVEL SECURITY;
