-- +goose Up

-- audience_mappings — tenant-scoped, named, reusable custom field mappings
-- ("connectors", ADR 0008 Feature 3). A provider ships files in an arbitrary
-- column layout; a tenant builds a named mapping (their lowercased source column
-- → our canonical field) from a small sample, then applies it on a real upload.
--
-- The mapping VALUES (canonical targets) are restricted at the store layer to the
-- fields we actually consume (id_value, id_type) — a provider can't map, and we
-- never store, columns we don't use. Mirrors audience_ingest_jobs (054) for RLS.
CREATE TABLE audience_mappings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    -- their lowercased source column → canonical field (id_value | id_type).
    mappings JSONB NOT NULL,
    -- id_type stamped on rows without an explicit id_type column.
    id_type TEXT NOT NULL DEFAULT 'user_id',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A tenant's mapping name is unique — re-saving the same name upserts.
    UNIQUE (account_id, name)
);

-- Account-facing listing.
CREATE INDEX idx_audience_mappings_account ON audience_mappings (account_id, created_at DESC);

ALTER TABLE audience_mappings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audience_mappings
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON audience_mappings;
DROP TABLE audience_mappings;
