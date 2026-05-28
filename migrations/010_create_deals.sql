-- +goose Up
CREATE TABLE deals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publisher_id UUID NOT NULL REFERENCES publishers(id),
    account_id UUID NOT NULL REFERENCES accounts(id),
    name TEXT NOT NULL,
    deal_type TEXT NOT NULL CHECK (deal_type IN ('open', 'pmp', 'pg', 'preferred')),
    price DECIMAL,
    price_currency TEXT DEFAULT 'USD',
    advertiser_ids UUID[] DEFAULT '{}',
    placement_ids UUID[] DEFAULT '{}',
    guaranteed_volume BIGINT, -- for PG deals
    start_date DATE,
    end_date DATE,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('draft', 'active', 'paused', 'ended')),
    deal_config JSONB DEFAULT '{}', -- video break positions, skip policy, etc.
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_deals_publisher ON deals (publisher_id);
CREATE INDEX idx_deals_account ON deals (account_id);
CREATE INDEX idx_deals_type ON deals (deal_type);

-- +goose Down
DROP TABLE deals;
