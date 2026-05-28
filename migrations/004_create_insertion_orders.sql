-- +goose Up
CREATE TABLE insertion_orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    name TEXT NOT NULL,
    budget DECIMAL NOT NULL,
    daily_budget DECIMAL,
    currency TEXT NOT NULL DEFAULT 'USD',
    start_date DATE NOT NULL,
    end_date DATE NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'paused', 'ended', 'archived')),
    objective TEXT CHECK (objective IN ('brand_awareness', 'performance', 'retargeting')),
    budget_rollover BOOLEAN NOT NULL DEFAULT false,
    budget_rollover_cap_pct INT DEFAULT 150,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_io_account ON insertion_orders (account_id);
CREATE INDEX idx_io_status ON insertion_orders (status);
CREATE INDEX idx_io_dates ON insertion_orders (start_date, end_date);

-- +goose Down
DROP TABLE insertion_orders;
