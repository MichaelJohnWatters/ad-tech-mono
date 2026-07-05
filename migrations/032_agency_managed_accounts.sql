-- +goose Up
-- Agency → managed-account links. A media-buying agency account may act on
-- behalf of the advertiser accounts assigned to it (staff-assigned). The
-- agency's own users authenticate with the agency account; act-as forwards a
-- managed advertiser account as the effective tenant on downstream calls.
--
-- agency_account_id is the agency's own account; managed_account_id is an
-- advertiser account it may act as. RLS scopes rows to the agency (its users
-- read their own links); staff/admin manage assignments via the platform key.
CREATE TABLE agency_managed_accounts (
    agency_account_id  UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    managed_account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agency_account_id, managed_account_id)
);

CREATE INDEX idx_ama_agency ON agency_managed_accounts (agency_account_id);
CREATE INDEX idx_ama_managed ON agency_managed_accounts (managed_account_id);

ALTER TABLE agency_managed_accounts ENABLE ROW LEVEL SECURITY;

-- The agency owns (reads) its own links. account_id is the current tenant.
CREATE POLICY tenant_isolation ON agency_managed_accounts
    USING (agency_account_id = current_setting('app.current_account_id', true)::uuid);

-- +goose Down
DROP TABLE agency_managed_accounts;
