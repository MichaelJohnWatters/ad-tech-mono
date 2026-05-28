-- +goose Up
CREATE TABLE creatives (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    name TEXT NOT NULL,
    format TEXT NOT NULL CHECK (format IN ('display', 'native', 'video', 'audio')),
    width INT,
    height INT,
    asset_url TEXT, -- Minio/S3 URL
    html_content TEXT,
    landing_url TEXT NOT NULL,
    duration_seconds INT, -- video/audio only
    review_status TEXT NOT NULL DEFAULT 'uploaded' CHECK (review_status IN (
        'uploaded', 'transcoding', 'auto_scanning', 'pending_review',
        'approved', 'rejected'
    )),
    rejection_reason TEXT,
    reviewed_by UUID REFERENCES team_members(id),
    reviewed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_creatives_account ON creatives (account_id);
CREATE INDEX idx_creatives_format ON creatives (format);
CREATE INDEX idx_creatives_review ON creatives (review_status);

-- Junction table: line items <-> creatives (many-to-many with rotation weight)
CREATE TABLE line_item_creatives (
    line_item_id UUID NOT NULL REFERENCES line_items(id) ON DELETE CASCADE,
    creative_id UUID NOT NULL REFERENCES creatives(id) ON DELETE CASCADE,
    weight INT NOT NULL DEFAULT 100,
    PRIMARY KEY (line_item_id, creative_id)
);

-- +goose Down
DROP TABLE line_item_creatives;
DROP TABLE creatives;
