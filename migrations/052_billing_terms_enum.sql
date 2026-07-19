-- +goose Up

-- Standardize advertiser_balances.payment_terms to exactly two values:
-- 'prepay' (default; paid up front via topup) | 'invoiced' (postpay; billed
-- monthly, may bid on credit up to credit_limit). Earlier data may carry the
-- pre-decision net_* terms (mirrored from the publisher vocabulary); collapse
-- anything that isn't 'prepay' to 'invoiced' so the CHECK holds, then enforce
-- the enum going forward. Additive: no column add (payment_terms + credit_limit
-- already exist from migration 012).
UPDATE advertiser_balances
   SET payment_terms = 'invoiced'
 WHERE payment_terms IS NOT NULL AND payment_terms <> 'prepay';

UPDATE advertiser_balances
   SET payment_terms = 'prepay'
 WHERE payment_terms IS NULL;

ALTER TABLE advertiser_balances
    ALTER COLUMN payment_terms SET DEFAULT 'prepay',
    ALTER COLUMN payment_terms SET NOT NULL;

ALTER TABLE advertiser_balances
    ADD CONSTRAINT advertiser_balances_payment_terms_check
    CHECK (payment_terms IN ('prepay', 'invoiced'));

-- +goose Down

ALTER TABLE advertiser_balances
    DROP CONSTRAINT IF EXISTS advertiser_balances_payment_terms_check;

ALTER TABLE advertiser_balances
    ALTER COLUMN payment_terms DROP NOT NULL;
