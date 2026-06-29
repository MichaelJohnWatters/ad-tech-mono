-- +goose Up
-- Secrets table: platform-managed auth material consumed by the warm-cache
-- pattern used elsewhere (campaigns, placements, deals, …). Each row is
-- one credential — opaque to the platform, interpreted by whichever
-- middleware/handler reads it.
--
-- Why a single generic table rather than per-purpose tables:
--   - Same management UI / lifecycle (create / rotate / revoke / audit)
--     for all secret types — operators learn one flow.
--   - Warm cache loader filter is just `WHERE purpose IN (...)` per service.
--   - Adding a new credential type (e.g. webhook_signing) doesn't need a
--     migration.
--
-- Existing `api_keys` (migration 003) stays separate: that's for per-user
-- self-service keys with a different lifecycle (advertisers / publishers
-- issue + revoke their own from the dashboard). `secrets` is for keys an
-- operator manages on the platform's behalf.
--
-- Rotation semantics: status `active` = currently the canonical value.
-- `rotating` = old value still accepted during a grace window so partners
-- and in-flight requests don't break instantly. `revoked` = no longer
-- accepted. Validators query for status IN ('active', 'rotating') and
-- accept any match.

CREATE TABLE secrets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,                 -- human label, e.g. "jwt-gateway-signing", "prebid-partner-acme"
    value      TEXT NOT NULL,                 -- secret material; encryption-at-rest TBD per overlay
    purpose    TEXT NOT NULL CHECK (purpose IN (
                   'jwt_signing',     -- JWT HS256/RS256 keys (gateway issues, services validate)
                   'hmac_tracker',    -- HMAC secrets for browser-fired tracker pixels
                   'partner_shared',  -- shared secrets for external partner authentication (Prebid, S2S)
                   'service_s2s',     -- shared secrets for internal service-to-service auth
                   'api_key'          -- operator-managed API keys for DSP/SSP CRUD authentication
               )),
    owner      TEXT NOT NULL DEFAULT 'platform', -- 'platform' for cross-service, or service name (e.g. 'dsp', 'ssp')
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN (
                   'active',
                   'rotating',  -- old value during grace window after rotation
                   'revoked'    -- no longer accepted by validators
               )),
    rotated_at TIMESTAMPTZ,                   -- when this row transitioned from active to rotating
    revokes_at TIMESTAMPTZ,                   -- when status will/did transition rotating → revoked
    expires_at TIMESTAMPTZ,                   -- natural expiry independent of rotation flow
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hot-path lookup: warm cache loaders fetch by (purpose, owner) on poll.
-- Filtered partial index keeps it cheap by excluding tombstones.
CREATE INDEX idx_secrets_purpose_owner ON secrets (purpose, owner) WHERE status != 'revoked';

-- Name lookup for the operator UI ("show me this secret") and for
-- auth middleware that maps a presented credential value back to a row.
CREATE INDEX idx_secrets_name ON secrets (name) WHERE status != 'revoked';

-- Value lookup for AuthAPIKey middleware: incoming header value → row.
-- Partial index on non-revoked rows; values are unique within (active+rotating)
-- by convention but not enforced at schema level (rotation transiently
-- creates two rows with the same logical name but different values).
CREATE INDEX idx_secrets_value ON secrets (value) WHERE status != 'revoked';

-- +goose Down
DROP TABLE IF EXISTS secrets;
