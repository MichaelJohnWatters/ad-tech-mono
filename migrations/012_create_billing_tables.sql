-- +goose Up

-- Advertiser balances
CREATE TABLE advertiser_balances (
    account_id UUID PRIMARY KEY REFERENCES accounts(id),
    balance DECIMAL NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT 'USD',
    credit_limit DECIMAL DEFAULT 0,
    payment_terms TEXT DEFAULT 'prepay',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Invoices
CREATE TABLE invoices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    total DECIMAL NOT NULL,
    currency TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'paid', 'overdue', 'cancelled')),
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    due_date DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_invoices_account ON invoices (account_id);

-- Publisher payouts
CREATE TABLE payouts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publisher_id UUID NOT NULL REFERENCES publishers(id),
    account_id UUID NOT NULL REFERENCES accounts(id),
    amount DECIMAL NOT NULL,
    currency TEXT NOT NULL,
    platform_fee DECIMAL NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'paid')),
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payouts_publisher ON payouts (publisher_id);

-- Adjustments (credits/debits for disputes, fraud refunds)
CREATE TABLE adjustments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    amount DECIMAL NOT NULL,
    currency TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('credit', 'debit')),
    reason TEXT NOT NULL,
    created_by UUID REFERENCES team_members(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_adjustments_account ON adjustments (account_id);

-- Budget reservations for CPC/CPA (reserve on win, settle on click/conversion)
CREATE TABLE budget_reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    line_item_id UUID NOT NULL REFERENCES line_items(id),
    trace_id TEXT NOT NULL,
    amount DECIMAL NOT NULL,
    currency TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'reserved' CHECK (status IN ('reserved', 'settled', 'released')),
    expires_at TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_reservations_line_item ON budget_reservations (line_item_id);
CREATE INDEX idx_reservations_status ON budget_reservations (status);
CREATE INDEX idx_reservations_trace ON budget_reservations (trace_id);

-- Double-entry accounting ledger
CREATE TABLE ledger_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_code TEXT NOT NULL, -- e.g. advertiser:{id}:balance, publisher:{id}:earnings, platform:revenue
    entry_type TEXT NOT NULL CHECK (entry_type IN ('debit', 'credit')),
    amount DECIMAL NOT NULL,
    currency TEXT NOT NULL,
    reference_type TEXT NOT NULL, -- impression, click, conversion, topup, payout, adjustment
    reference_id TEXT NOT NULL,
    trace_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ledger_account ON ledger_entries (account_code);
CREATE INDEX idx_ledger_reference ON ledger_entries (reference_type, reference_id);
CREATE INDEX idx_ledger_created ON ledger_entries (created_at);

-- Exchange rates
CREATE TABLE exchange_rates (
    base_currency TEXT NOT NULL DEFAULT 'USD',
    target_currency TEXT NOT NULL,
    rate DECIMAL NOT NULL,
    effective_date DATE NOT NULL,
    source TEXT NOT NULL DEFAULT 'ecb',
    PRIMARY KEY (base_currency, target_currency, effective_date)
);

-- +goose Down
DROP TABLE exchange_rates;
DROP TABLE ledger_entries;
DROP TABLE budget_reservations;
DROP TABLE adjustments;
DROP TABLE payouts;
DROP TABLE invoices;
DROP TABLE advertiser_balances;
