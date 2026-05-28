-- +goose Up
CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_by UUID NOT NULL REFERENCES team_members(id),
    key_hash TEXT NOT NULL, -- bcrypt hash, never store plaintext
    name TEXT NOT NULL,
    tier TEXT NOT NULL DEFAULT 'standard' CHECK (tier IN ('standard', 'enterprise')),
    scoped_permissions TEXT[] DEFAULT '{}',
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_api_keys_account ON api_keys (account_id);

-- +goose Down
DROP TABLE api_keys;
