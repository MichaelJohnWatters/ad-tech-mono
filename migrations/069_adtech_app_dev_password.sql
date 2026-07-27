-- +goose Up
-- +goose StatementBegin

-- Security #77 finalize: migration 067 creates adtech_app LOGIN but leaves the
-- password OUT OF BAND. That was fine while the flip was being tested (the dev
-- password was set via a manual ALTER ROLE), but once values.yaml points every
-- app service's DATABASE_URL at adtech_app, a FRESH `make stack-up` must bring
-- the role up already able to authenticate — otherwise every pod CrashLoops on
-- "password authentication failed".
--
-- This sets the LOCAL DEV password, and ONLY when none is set yet (rolpassword
-- IS NULL), so it never clobbers a real password a prod operator has already
-- rotated onto the role. The value matches the dev DATABASE_URL in
-- k8s/helm/adtech/values.yaml (the plaintext-dev-creds-in-local convention).
--
-- PROD/STAGING: this dev default is NOT acceptable there. The deploy runbook
-- (docs/DEPLOY.md) rotates it immediately after migrate:
--     ALTER ROLE adtech_app PASSWORD '<value from the SOPS secret>';
-- and values-{staging,prod}.yaml's DATABASE_URL sources that same secret. See
-- docs/RLS_FLIP_PLAN.md → "Finalize the flip".
DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'adtech_app' AND rolpassword IS NULL) THEN
    ALTER ROLE adtech_app PASSWORD 'adtech-app-local';
  END IF;
END
$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Clear the password (best-effort; the role itself is dropped by 067's Down).
DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'adtech_app') THEN
    ALTER ROLE adtech_app PASSWORD NULL;
  END IF;
END
$$;
-- +goose StatementEnd
