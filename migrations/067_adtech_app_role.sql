-- +goose Up
-- +goose StatementBegin

-- Security #77: the limited application DB role (see docs/SECURITY_HARDENING.md
-- §3). The app currently connects as the superuser `adtech` (rolsuper,
-- rolbypassrls), so every RLS tenant_isolation policy is a no-op safety net.
-- This migration creates `adtech_app` — LOGIN, NOSUPERUSER, NOBYPASSRLS — so
-- that once DATABASE_URL points the services at it, RLS actually bites and the
-- cross-tenant warm-cache loaders go through the audited app.platform_read hatch
-- (migration 065 + the QueryPlatform store helpers).
--
-- Pure DDL, additive and a NO-OP until the deploy flips DATABASE_URL: the role
-- exists but nothing connects as it yet. Its PASSWORD is set OUT OF BAND (a SOPS
-- secret in prod/staging; a known dev value locally) — deliberately not in this
-- migration, which also runs in prod. The migrate job connects as the owner/
-- superuser, so CREATE ROLE + GRANT + ALTER DEFAULT PRIVILEGES all succeed and
-- the default privileges cover tables created by later migrations.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'adtech_app') THEN
    CREATE ROLE adtech_app LOGIN NOSUPERUSER NOBYPASSRLS;
  ELSE
    -- Converge an existing role to the intended posture (idempotent re-run).
    ALTER ROLE adtech_app LOGIN NOSUPERUSER NOBYPASSRLS;
  END IF;
END $$;

GRANT USAGE ON SCHEMA public TO adtech_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO adtech_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO adtech_app;

-- Future tables/sequences (later migrations, created by the owner) auto-grant.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO adtech_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO adtech_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'adtech_app') THEN
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
      REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM adtech_app;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
      REVOKE USAGE, SELECT ON SEQUENCES FROM adtech_app;
    EXECUTE 'DROP OWNED BY adtech_app';
    EXECUTE 'DROP ROLE adtech_app';
  END IF;
END $$;
-- +goose StatementEnd
