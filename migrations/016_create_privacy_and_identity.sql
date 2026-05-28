-- +goose Up

-- Identity graph edges (cross-publisher, cross-device links)
CREATE TABLE identity_graph (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    linked_id TEXT NOT NULL,
    source TEXT NOT NULL, -- publisher, hashed_email, device, probabilistic
    link_type TEXT NOT NULL, -- cross_publisher, cross_device, crm_match
    confidence DECIMAL NOT NULL DEFAULT 1.0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ
);

CREATE INDEX idx_identity_user ON identity_graph (user_id);
CREATE INDEX idx_identity_linked ON identity_graph (linked_id);
CREATE UNIQUE INDEX idx_identity_pair ON identity_graph (user_id, linked_id, source);

-- User opt-out registry
CREATE TABLE opt_out_registry (
    user_id TEXT PRIMARY KEY,
    level INT NOT NULL CHECK (level IN (1, 2, 3)),
    source TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    verified_at TIMESTAMPTZ,
    systems_completed JSONB DEFAULT '{}'
);

-- Saved reports (per account)
CREATE TABLE saved_reports (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    query_config JSONB NOT NULL,
    schedule TEXT, -- cron expression or NULL for manual
    delivery TEXT DEFAULT 'none', -- email, webhook, none
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_saved_reports_account ON saved_reports (account_id);

-- ads.txt cache
CREATE TABLE ads_txt_cache (
    domain TEXT PRIMARY KEY,
    entries JSONB NOT NULL,
    last_fetched TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'valid' CHECK (status IN ('valid', 'missing', 'error')),
    last_changed TIMESTAMPTZ
);

-- Fraud blocklists
CREATE TABLE fraud_blocklists (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type TEXT NOT NULL CHECK (type IN ('ip', 'ua', 'domain', 'app_bundle')),
    value TEXT NOT NULL,
    reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_blocklist_type_value ON fraud_blocklists (type, value);

-- Deployment ledger
CREATE TABLE deployment_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    timestamp TIMESTAMPTZ NOT NULL DEFAULT now(),
    service TEXT NOT NULL,
    event_type TEXT NOT NULL,
    image_tag TEXT NOT NULL,
    git_sha TEXT NOT NULL,
    git_branch TEXT,
    previous_image_tag TEXT,
    previous_git_sha TEXT,
    triggered_by TEXT NOT NULL,
    environment TEXT NOT NULL,
    replicas INT,
    status TEXT NOT NULL DEFAULT 'started',
    duration_ms INT,
    notes TEXT,
    metadata JSONB
);

CREATE INDEX idx_deploy_service_time ON deployment_ledger (service, timestamp DESC);
CREATE INDEX idx_deploy_env_time ON deployment_ledger (environment, timestamp DESC);

-- +goose Down
DROP TABLE deployment_ledger;
DROP TABLE fraud_blocklists;
DROP TABLE ads_txt_cache;
DROP TABLE saved_reports;
DROP TABLE opt_out_registry;
DROP TABLE identity_graph;
