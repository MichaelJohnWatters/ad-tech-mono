-- +goose Up
-- G7 (attribution gaps) — per-advertiser conversion signing keys.
--
-- WHY: today ONE shared HMAC key (tracker.signing_key + the hmac_tracker
-- overlap set) signs and validates every tracker beacon, including the S2S
-- CONVERSION postback that triggers CPA billing. Any party holding that shared
-- key can forge a signed conversion for ANY advertiser and rack up their spend —
-- the cross-advertiser billing-fraud surface Phase 0 was careful about is only
-- half-closed. The fix is to issue each advertiser its OWN conversion-signing
-- key and validate /v1/t/conv against THAT advertiser's key (by advid), so
-- advertiser B can't forge a conversion billed to advertiser A.
--
-- Platform-signed beacons (impression/click/view, signed by OUR ad server) stay
-- on the platform key — only the advertiser-HELD conversion postback goes
-- per-advertiser. New purpose 'hmac_conversion' distinguishes the two.
--
-- account_id is nullable: NULL = platform-wide (every existing row keeps its
-- current meaning — no backfill needed), non-NULL = scoped to one advertiser.
-- ON DELETE CASCADE so deleting an advertiser cleans up its keys.
--
-- No RLS: the tracker validator must load EVERY advertiser's conversion key into
-- its warm cache to validate any incoming conversion, so this stays a platform
-- table (like the rest of secrets). Isolation is enforced at issuance (a caller
-- can only mint/rotate a key for its own account) and at validation (a
-- conversion is checked only against ITS advid's key), not by row-level reads.
ALTER TABLE secrets ADD COLUMN IF NOT EXISTS account_id UUID
    REFERENCES accounts(id) ON DELETE CASCADE;

-- CHECK constraints can't be extended in place → drop + recreate with the extra
-- value (mirrors migrations 056 / 061).
ALTER TABLE secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
    'jwt_signing',      -- JWT HS256/RS256 keys (gateway issues, services validate)
    'hmac_tracker',     -- HMAC secrets for browser-fired tracker pixels (platform key)
    'hmac_conversion',  -- per-advertiser HMAC key for S2S conversion postbacks (G7)
    'partner_shared',   -- shared secrets for external partner authentication (Prebid, S2S)
    'service_s2s',      -- shared secrets for internal service-to-service auth
    'api_key',          -- operator-managed API keys for DSP/SSP CRUD authentication
    'pgp_private',      -- armored OpenPGP private key for audience-file decrypt-on-ingest (ADR 0008)
    'adcert_ed25519'    -- Ed25519 private key for signing outbound OpenRTB bid requests (Phase I)
));

-- Hot-path lookup for the tracker validator: given a conversion's advid, find
-- that advertiser's non-revoked conversion keys.
CREATE INDEX IF NOT EXISTS idx_secrets_purpose_account
    ON secrets (purpose, account_id) WHERE status != 'revoked';

-- +goose Down
DROP INDEX IF EXISTS idx_secrets_purpose_account;
-- Any 'hmac_conversion' rows must be removed before the constraint recreate.
DELETE FROM secrets WHERE purpose = 'hmac_conversion';
ALTER TABLE secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
    'jwt_signing',
    'hmac_tracker',
    'partner_shared',
    'service_s2s',
    'api_key',
    'pgp_private',
    'adcert_ed25519'
));
ALTER TABLE secrets DROP COLUMN IF EXISTS account_id;
