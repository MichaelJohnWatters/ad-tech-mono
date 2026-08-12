-- +goose Up
-- Phase 11 #112 slice 2: give external partners a self-serve login. A partner
-- gets its own `accounts` row of the NEW type 'partner' (staff-provisioned), and
-- the registry row links to it so the partner portal can show the partner its own
-- onboarding record. (Accounts of type partner have a minimal permission set —
-- see pkg/auth partner:owner.)

-- Allow the new account type. The inline CHECK from migration 001 is auto-named
-- accounts_type_check.
ALTER TABLE accounts DROP CONSTRAINT accounts_type_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_type_check
    CHECK (type IN ('advertiser', 'publisher', 'agency', 'staff', 'admin', 'partner'));

-- Link a registry row to its partner login account (nullable until provisioned;
-- one account ↔ one partner row).
ALTER TABLE partners ADD COLUMN account_id UUID REFERENCES accounts(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX idx_partners_account ON partners (account_id) WHERE account_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_partners_account;
ALTER TABLE partners DROP COLUMN account_id;
ALTER TABLE accounts DROP CONSTRAINT accounts_type_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_type_check
    CHECK (type IN ('advertiser', 'publisher', 'agency', 'staff', 'admin'));
