-- +goose Up
CREATE TABLE audience_segments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    name TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN (
        'first_party', 'behavioral', 'lookalike', 'suppression',
        'retargeting', 'composite', 'predictive', 'cdp_imported'
    )),
    size_estimate BIGINT DEFAULT 0,
    logic_json JSONB, -- for composite segments: boolean expression
    suppression BOOLEAN NOT NULL DEFAULT false,
    taxonomy_categories TEXT[] DEFAULT '{}', -- IAB Audience Taxonomy
    status TEXT NOT NULL DEFAULT 'active',
    source TEXT, -- crm_upload, platform, cdp:mparticle, etc.
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_segments_account ON audience_segments (account_id);
CREATE INDEX idx_segments_type ON audience_segments (type);

-- +goose Down
DROP TABLE audience_segments;
