# privacy-delete (Job)

One-shot executor for Level-3 (full deletion) GDPR requests: load pending
`opt_out_registry` rows (level=3, `completed_at IS NULL`) → purge every
user-keyed row → mark completed → announce `adtech.privacy.deletion_completed`
(`events.SubjectPrivacyCompleted`) → exit.

## What it purges (per user)

- Postgres, one tx: `identity_graph` (either endpoint — `user_id OR linked_id`)
  + `audience_segment_members`
- Extras (`pkg/privacydelete/extras.go`, wired by `BuildExtras`): ClickHouse
  `freq_cap_blocks` + `behaviour_signals`/`profile_signals`, then re-exports the
  exact affected hours to Parquet so the derived archive loses the user too (ADR 0006)

## How it runs

- **Production path: NOT a standalone CronJob.** Runs in-process as the
  `privacy-delete` step of the batch-conductor chain (`pkg/batch/chain.go`,
  hourly CronJob `10 * * * *` in `k8s/helm/adtech/values.yaml`), immediately
  followed by `privacy-verify`. This binary is the manual escape hatch
  (`go run ./cmd/privacy-delete`); sibling `cmd/privacy-verify` audits residuals
  and stamps `verified_at`.
- Core logic lives in `pkg/privacydelete` (Deleter / Verifier / PostgresStore) —
  this binary is thin wiring. 5-minute run timeout.

## Idempotency / failure model

- Per-user failure is logged and skipped; the registry row stays pending, so the
  next run retries. PG deletes are idempotent re-runs.
- An extra-purger failure fails the whole `PurgeUser` (fail-safe, never
  fail-open) — the PG tx has already committed, but the row stays pending and
  the full purge re-runs.
- NATS is optional: deletion proceeds without the completion announcement.
- ClickHouse unreachable → WARN + skip extras (those tables only exist on the CH
  backend). Empty S3 endpoint → SignalsPurger skips the Parquet re-export (the
  next scheduled export re-derives from ClickHouse).

## CRITICAL: RLS platform hatch

A purge spans every tenant that ever saw the user — a cross-tenant write.
`audience_segment_members` carries tenant RLS (mig 019 + mig 065 hatch); under
the NOBYPASSRLS app role a bare DELETE/SELECT matches ZERO rows and silently
certifies a purge that deleted nothing. `PurgeUser` AND `Residual` therefore set
tx-local `SELECT set_config('app.platform_read','on',true)`. Any new user-data
table added to the purge needs the same treatment.

## Config keys (`pkg/config/keys/privacydelete.go`)

- `privacy_delete.nats_url` — completion announcement bus
- `privacy_delete.pipeline_url` — vestige of the retired Delta LakePurger
  (ADR 0006); unused in the live wiring
- Extras reuse `keys.Reporting.ClickHouse*`, `keys.S3.*`, `keys.Pipeline.DatalakeBucket`

## Pointers

- `docs/PLAN.md` -> "9a. User Opt-Out and Data Deletion System" (the 3-level model)
- `docs/PLAN.md` -> "Privacy and Consent", "NATS Subjects for Opt-Out"
- `cmd/CLAUDE.md` -> `batch-conductor/` row (chain ordering: delete runs before verify)
