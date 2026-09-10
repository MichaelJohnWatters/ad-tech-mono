-- +goose Up
-- Per-account SSO config (PLAN Phase 11, item 110): an account's users can log in
-- via their own OIDC IdP, alongside password auth. One config row per account.
-- client_secret is stored here (RLS-protected, never serialized to the config API);
-- the secrets-table home is a documented hardening follow-up.
CREATE TABLE sso_configurations (
    account_id      UUID PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    enabled         BOOLEAN NOT NULL DEFAULT false,
    protocol        TEXT NOT NULL DEFAULT 'oidc' CHECK (protocol IN ('oidc')),
    issuer          TEXT NOT NULL DEFAULT '',   -- OIDC issuer URL (discovery base)
    client_id       TEXT NOT NULL DEFAULT '',
    client_secret   TEXT NOT NULL DEFAULT '',   -- confidential-client secret (never serialized out)
    allowed_domains TEXT[] NOT NULL DEFAULT '{}', -- email domains permitted to JIT-provision
    default_role    TEXT NOT NULL DEFAULT 'viewer'
                      CHECK (default_role IN ('viewer', 'analyst', 'ad_ops', 'finance', 'manager')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- RLS: the owner reads/writes their own row (tenant GUC); the LOGIN path reads it
-- cross-tenant (pre-auth) via the platform_read hatch. NULLIF-guarded for the
-- empty-string GUC left on pooled connections (mig 087).
ALTER TABLE sso_configurations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sso_configurations
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE sso_configurations;
