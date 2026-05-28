-- +goose Up
-- Line items are what "campaigns" mean throughout the platform.
-- campaign_id in events and Redis keys refers to line_item.id.
CREATE TABLE line_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    insertion_order_id UUID NOT NULL REFERENCES insertion_orders(id),
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft', 'submitted', 'in_review', 'rejected', 'approved',
        'live', 'paused', 'ended', 'archived'
    )),
    format TEXT NOT NULL DEFAULT 'display' CHECK (format IN (
        'display', 'native', 'video', 'audio'
    )),
    bid_strategy TEXT NOT NULL DEFAULT 'cpm' CHECK (bid_strategy IN (
        'cpm', 'cpc', 'cpa', 'vcpm', 'cpcv'
    )),
    base_bid DECIMAL NOT NULL,
    bid_currency TEXT NOT NULL DEFAULT 'USD',
    sub_budget DECIMAL, -- NULL = shared IO pool, set = capped
    daily_budget DECIMAL,
    pacing_mode TEXT NOT NULL DEFAULT 'even' CHECK (pacing_mode IN (
        'even', 'asap', 'front_loaded'
    )),
    shading_mode TEXT NOT NULL DEFAULT 'moderate' CHECK (shading_mode IN (
        'aggressive', 'moderate', 'conservative', 'disabled'
    )),
    creative_rotation TEXT NOT NULL DEFAULT 'bandit' CHECK (creative_rotation IN (
        'even', 'weighted', 'bandit', 'sequential'
    )),
    timezone TEXT NOT NULL DEFAULT 'UTC',
    rejection_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_li_account ON line_items (account_id);
CREATE INDEX idx_li_io ON line_items (insertion_order_id);
CREATE INDEX idx_li_status ON line_items (status);

-- +goose Down
DROP TABLE line_items;
