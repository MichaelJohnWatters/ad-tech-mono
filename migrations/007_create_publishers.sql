-- +goose Up
CREATE TABLE publishers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    name TEXT NOT NULL,
    domain TEXT NOT NULL,
    currency TEXT NOT NULL DEFAULT 'USD',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'suspended', 'closed')),
    -- Revenue share contract
    revshare_model TEXT NOT NULL DEFAULT 'fixed' CHECK (revshare_model IN (
        'fixed', 'tiered', 'guaranteed_minimum', 'deal_type', 'hybrid'
    )),
    revshare_config JSONB NOT NULL DEFAULT '{"fee_pct": 20}',
    payment_terms TEXT NOT NULL DEFAULT 'net_30',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_publishers_account ON publishers (account_id);
CREATE INDEX idx_publishers_domain ON publishers (domain);

-- +goose Down
DROP TABLE publishers;
