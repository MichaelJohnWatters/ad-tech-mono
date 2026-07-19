-- +goose Up

-- Advertiser-defined conversion events: the setup layer in front of the tracker's
-- /v1/t/conv endpoint. An advertiser names a conversion (purchase, signup, lead,
-- custom) with a default value + currency, and the portal hands back an
-- embeddable pixel/snippet keyed to that event_type. The tracker already records
-- conversions (trace_id, conversion_type, revenue, currency) into analytics +
-- CPA settle — this table just gives advertisers a self-serve way to define the
-- named events and generate the embed.
CREATE TABLE conversion_configs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    event_type TEXT NOT NULL CHECK (event_type IN ('purchase', 'signup', 'lead', 'custom')),
    default_value NUMERIC NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT 'USD',
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One config per (account, name): re-creating the same name for a tenant is a
-- conflict, and it's the natural lookup key from the portal.
CREATE UNIQUE INDEX idx_conversion_configs_account_name ON conversion_configs (account_id, name);

ALTER TABLE conversion_configs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON conversion_configs
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON conversion_configs;
DROP TABLE conversion_configs;
