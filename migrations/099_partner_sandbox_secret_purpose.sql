-- +goose Up
-- Phase 11 #112 slice 2b: a partner's self-serve SANDBOX API key. Distinct from
-- 'partner_shared' (platform-owned Prebid/S2S secrets) — 'partner_sandbox' keys
-- are per-partner-account, account-scoped, and the partner generates/rotates/
-- revokes them itself from its portal (same lifecycle as hmac_conversion).
--
-- CHECK constraints can't be extended in place → drop + recreate with the extra
-- value (mirrors migrations 056/061/071).
ALTER TABLE secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
    'jwt_signing',
    'hmac_tracker',
    'hmac_conversion',
    'partner_shared',
    'partner_sandbox',  -- per-partner self-serve sandbox API key (#112)
    'service_s2s',
    'api_key',
    'pgp_private',
    'adcert_ed25519'
));

-- +goose Down
DELETE FROM secrets WHERE purpose = 'partner_sandbox';
ALTER TABLE secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
    'jwt_signing',
    'hmac_tracker',
    'hmac_conversion',
    'partner_shared',
    'service_s2s',
    'api_key',
    'pgp_private',
    'adcert_ed25519'
));
