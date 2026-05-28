-- +goose Up
CREATE TABLE targeting_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    line_item_id UUID NOT NULL REFERENCES line_items(id) ON DELETE CASCADE,
    account_id UUID NOT NULL REFERENCES accounts(id),
    include_geo TEXT[] DEFAULT '{}',
    exclude_geo TEXT[] DEFAULT '{}',
    include_device TEXT[] DEFAULT '{}',
    exclude_device TEXT[] DEFAULT '{}',
    include_os TEXT[] DEFAULT '{}',
    include_segments TEXT[] DEFAULT '{}',
    exclude_segments TEXT[] DEFAULT '{}',
    include_domains TEXT[] DEFAULT '{}',
    exclude_domains TEXT[] DEFAULT '{}',
    include_app_bundles TEXT[] DEFAULT '{}',
    exclude_app_bundles TEXT[] DEFAULT '{}',
    include_categories TEXT[] DEFAULT '{}',
    exclude_categories TEXT[] DEFAULT '{}',
    include_languages TEXT[] DEFAULT '{}',
    include_inventory_type TEXT[] DEFAULT '{}', -- site, app
    include_keywords TEXT[] DEFAULT '{}',
    exclude_keywords TEXT[] DEFAULT '{}',
    bid_modifiers JSONB DEFAULT '{}', -- {"device": {"mobile": 20}, "geo_country": {"UK": 15}, ...}
    frequency_caps JSONB DEFAULT '[]', -- [{"dimension": "line_item", "window": "day", "limit": 5}, ...]
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_targeting_line_item ON targeting_rules (line_item_id);
CREATE INDEX idx_targeting_account ON targeting_rules (account_id);

-- +goose Down
DROP TABLE targeting_rules;
