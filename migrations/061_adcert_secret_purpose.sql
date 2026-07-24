-- +goose Up
-- Phase I — ads.cert Ed25519 signing key moves into the secrets store so it gets
-- HOT + OVERLAPPING rotation (active + rotating both usable during a grace
-- window), the same as jwt_signing / hmac_tracker. The exchange signs outbound
-- bid requests with the ACTIVE adcert_ed25519 private key and publishes the
-- public halves of ALL non-revoked keys at /v1/adcert/key (a keyset), so DSPs
-- verify against either during a rotation. Value = base64 raw-url Ed25519
-- private key (seed+public form), same encoding as exchange.adcert_sign_key.
--
-- CHECK constraints can't be extended in place → drop + recreate with the extra
-- value (mirrors migration 056 for pgp_private).
ALTER TABLE secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
    'jwt_signing',      -- JWT HS256/RS256 keys (gateway issues, services validate)
    'hmac_tracker',     -- HMAC secrets for browser-fired tracker pixels
    'partner_shared',   -- shared secrets for external partner authentication (Prebid, S2S)
    'service_s2s',      -- shared secrets for internal service-to-service auth
    'api_key',          -- operator-managed API keys for DSP/SSP CRUD authentication
    'pgp_private',      -- armored OpenPGP private key for audience-file decrypt-on-ingest (ADR 0008)
    'adcert_ed25519'    -- Ed25519 private key for signing outbound OpenRTB bid requests (Phase I)
));

-- +goose Down
-- Restore the pre-adcert constraint. Any 'adcert_ed25519' rows must be removed
-- first or the recreate fails.
ALTER TABLE secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
    'jwt_signing',
    'hmac_tracker',
    'partner_shared',
    'service_s2s',
    'api_key',
    'pgp_private'
));
