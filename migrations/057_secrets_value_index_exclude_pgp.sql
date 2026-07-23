-- +goose Up

-- The by-value lookup index (idx_secrets_value) is a plain btree on secrets.value
-- for validating a presented short secret (api_key / hmac_tracker / service_s2s /
-- partner_shared) back to its row. A PGP private key (ADR 0008, purpose
-- 'pgp_private') is multi-KB armored text — larger than the btree's 2704-byte
-- max, so inserting one fails. PGP keys are only ever looked up BY PURPOSE
-- (LookupActiveByPurpose / a purpose-filtered query), never by value, so exclude
-- them from the value index.
DROP INDEX IF EXISTS idx_secrets_value;
CREATE INDEX idx_secrets_value ON secrets (value)
    WHERE status != 'revoked' AND purpose != 'pgp_private';

-- +goose Down
DROP INDEX IF EXISTS idx_secrets_value;
CREATE INDEX idx_secrets_value ON secrets (value) WHERE status != 'revoked';
