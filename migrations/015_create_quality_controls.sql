-- +goose Up

CREATE TABLE quality_controls (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publisher_id UUID NOT NULL REFERENCES publishers(id) ON DELETE CASCADE,
    account_id UUID NOT NULL REFERENCES accounts(id),
    type TEXT NOT NULL CHECK (type IN (
        'advertiser_blocklist', 'category_blocklist', 'creative_blocklist',
        'advertiser_allowlist', 'domain_blocklist'
    )),
    values TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_qc_publisher ON quality_controls (publisher_id);

CREATE TABLE creative_review_queue (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    creative_id UUID NOT NULL REFERENCES creatives(id),
    account_id UUID NOT NULL REFERENCES accounts(id),
    type TEXT NOT NULL CHECK (type IN ('creative', 'advertiser', 'publisher', 'credit')),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    auto_scan_result JSONB,
    reviewed_by UUID REFERENCES team_members(id),
    reviewed_at TIMESTAMPTZ,
    rejection_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_review_status ON creative_review_queue (status);

-- +goose Down
DROP TABLE creative_review_queue;
DROP TABLE quality_controls;
