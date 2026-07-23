-- +goose Up
-- ADR 0008 Feature 1 — PGP decrypt-on-ingest. Providers encrypt audience files
-- to the platform PUBLIC key; we decrypt on ingest with the PRIVATE key. The
-- armored private key lives in the secrets store as one platform-owned row with
-- the new purpose 'pgp_private' (same active/rotating/revoked model as
-- jwt_signing, so the keypair can rotate without breaking in-flight files —
-- decrypt tries every non-revoked private key).
--
-- CHECK constraints can't be extended in place, so drop + recreate with the
-- existing enum values plus 'pgp_private'.
ALTER TABLE secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
    'jwt_signing',     -- JWT HS256/RS256 keys (gateway issues, services validate)
    'hmac_tracker',    -- HMAC secrets for browser-fired tracker pixels
    'partner_shared',  -- shared secrets for external partner authentication (Prebid, S2S)
    'service_s2s',     -- shared secrets for internal service-to-service auth
    'api_key',         -- operator-managed API keys for DSP/SSP CRUD authentication
    'pgp_private'      -- armored OpenPGP private key for audience-file decrypt-on-ingest (ADR 0008)
));

-- +goose Down
-- Restore the pre-0008 constraint. Any 'pgp_private' rows must be removed first
-- or the recreate fails; revoke rather than accept ciphertext keys silently.
ALTER TABLE secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
    'jwt_signing',
    'hmac_tracker',
    'partner_shared',
    'service_s2s',
    'api_key'
));
