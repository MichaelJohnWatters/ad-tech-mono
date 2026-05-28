-- +goose Up

CREATE TABLE webhooks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    events TEXT[] NOT NULL, -- e.g. {'budget.depleted', 'invoice.generated'}
    secret TEXT NOT NULL, -- HMAC signing secret
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_webhooks_account ON webhooks (account_id);

CREATE TABLE webhook_deliveries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_id UUID NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL,
    response_status INT,
    response_body TEXT,
    attempt INT NOT NULL DEFAULT 1,
    success BOOLEAN NOT NULL DEFAULT false,
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_webhook_deliveries_webhook ON webhook_deliveries (webhook_id);

CREATE TABLE notification_preferences (
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    email BOOLEAN NOT NULL DEFAULT true,
    webhook BOOLEAN NOT NULL DEFAULT false,
    in_app BOOLEAN NOT NULL DEFAULT true,
    PRIMARY KEY (account_id, event_type)
);

-- +goose Down
DROP TABLE notification_preferences;
DROP TABLE webhook_deliveries;
DROP TABLE webhooks;
