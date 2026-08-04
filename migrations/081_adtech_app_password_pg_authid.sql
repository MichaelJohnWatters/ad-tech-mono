-- +goose Up
-- +goose StatementBegin

-- Fresh-boot fix for migration 069, found on the first TRUE fresh-disk
-- stack-up since the RLS flip: 069 gated the dev-password ALTER on
-- pg_roles.rolpassword IS NULL, but the pg_roles VIEW masks rolpassword as the
-- literal '********' for EVERY role — password or not — so the predicate was
-- never true and the ALTER never ran. Every app pod then failed 28P01 on a
-- fresh database (it "worked" before only because the password had been set
-- manually on the old, since-purged PVC). The real password column is
-- pg_authid.rolpassword, readable here because migrations run as the postgres
-- superuser.
--
-- Same never-clobber semantics as 069: set the LOCAL DEV password ONLY when no
-- password is set, so a rotated prod/staging password is never overwritten.
DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_authid WHERE rolname = 'adtech_app' AND rolpassword IS NULL) THEN
    ALTER ROLE adtech_app PASSWORD 'adtech-app-local';
  END IF;
END
$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: reverting a password heal would re-break fresh-boot auth; 069's Down
-- (password NULL) plus 067's Down (drop role) own teardown.
SELECT 1;
-- +goose StatementEnd
