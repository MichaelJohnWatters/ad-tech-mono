-- +goose Up
-- Drop the secrets.account_id → accounts foreign key added in migration 071.
--
-- The FK's ON DELETE CASCADE was a nicety (deleting an advertiser would auto-clean
-- its per-advertiser hmac_conversion keys). But it has a harmful side effect: it
-- puts `secrets` into the cascade of `TRUNCATE accounts CASCADE`, which the dev /
-- e2e reset (cmd/gateway/reset.go, tests/e2e/harness/reset.go) runs. So every
-- reset now wipes the ENTIRE secrets table — including the platform api_key /
-- jwt_signing / hmac_tracker rows that were NEVER touched before 071 — and the
-- reseed + warm-cache reload races that wipe, intermittently breaking API-key auth
-- (401) and HMAC validation right after a reset.
--
-- Per-advertiser conversion keys are matched purely on the account_id COLUMN
-- (tracker sigKeysForAdvertiser → NonRevokedByPurposeAndAccount); the FK is not
-- needed for correctness. An orphaned key (advertiser later deleted) simply never
-- matches a live account, which is harmless. Keep the column + the
-- (purpose, account_id) index from 071.
ALTER TABLE secrets DROP CONSTRAINT IF EXISTS secrets_account_id_fkey;

-- +goose Down
ALTER TABLE secrets ADD CONSTRAINT secrets_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE;
