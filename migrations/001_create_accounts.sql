-- +goose Up
CREATE TABLE accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    email TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL CHECK (type IN ('advertiser', 'publisher', 'agency', 'staff', 'admin')),
    currency TEXT NOT NULL DEFAULT 'USD',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'closed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_accounts_type ON accounts (type);
CREATE INDEX idx_accounts_email ON accounts (email);

-- +goose Down
DROP TABLE accounts;
