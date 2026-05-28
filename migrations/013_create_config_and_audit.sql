-- +goose Up

-- Live runtime configuration (dashboard-editable)
CREATE TABLE config (
    key TEXT PRIMARY KEY,
    value JSONB NOT NULL,
    service TEXT NOT NULL,
    description TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT
);

-- Immutable audit log (NO UPDATE/DELETE allowed)
CREATE TABLE audit_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID REFERENCES accounts(id),
    timestamp TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_id TEXT NOT NULL,
    actor_ip TEXT,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    changes JSONB DEFAULT '[]',
    reason TEXT
);

CREATE INDEX idx_audit_account ON audit_log (account_id);
CREATE INDEX idx_audit_resource ON audit_log (resource_type, resource_id);
CREATE INDEX idx_audit_actor ON audit_log (actor_id);
CREATE INDEX idx_audit_timestamp ON audit_log (timestamp);

-- Prevent UPDATE/DELETE on audit_log (immutable)
REVOKE UPDATE, DELETE ON audit_log FROM PUBLIC;

-- +goose Down
DROP TABLE audit_log;
DROP TABLE config;
