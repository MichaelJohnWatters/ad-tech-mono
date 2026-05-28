-- +goose Up
CREATE TABLE placements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publisher_id UUID NOT NULL REFERENCES publishers(id) ON DELETE CASCADE,
    account_id UUID NOT NULL REFERENCES accounts(id),
    name TEXT NOT NULL,
    format TEXT NOT NULL CHECK (format IN ('display', 'native', 'video', 'audio', 'dooh')),
    width INT,
    height INT,
    floor_price DECIMAL NOT NULL DEFAULT 0,
    floor_currency TEXT NOT NULL DEFAULT 'USD',
    page_url_pattern TEXT,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    floor_config JSONB DEFAULT '{}', -- time-based, device-based, geo-based floors
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_placements_publisher ON placements (publisher_id);
CREATE INDEX idx_placements_account ON placements (account_id);
CREATE INDEX idx_placements_format ON placements (format);

-- +goose Down
DROP TABLE placements;
