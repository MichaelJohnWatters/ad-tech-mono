# privacy-verify (Job)

One-shot auditor for completed Level-3 (full-deletion) privacy requests — the verify tail of the GDPR purge. Re-checks every completed-but-unverified `opt_out_registry` user for residual rows, stamps `verified_at` when clean, and fails loudly when data survived a deletion.

## What it does

- `privacydelete.Verifier.RunUnverified`: loads `opt_out_registry` rows (`level=3`, completed, `verified_at IS NULL`), then residual-checks each user across:
  - Postgres: `identity_graph` (`user_id` OR `linked_id`) + `audience_segment_members`
  - Extras via `privacydelete.BuildExtras`: ClickHouse `freq_cap_blocks` (FreqCapPurger) + `behaviour_signals`/`profile_signals` (SignalsPurger). ClickHouse unreachable → WARN + those checks skipped.
- Clean → stamp `verified_at`. Residual → ERROR log, row stays unverified (retried next run).
- Exit codes: 0 clean · 1 run error · **2 = residual PII found** — non-zero so a scheduled run surfaces as failed. 5-minute context timeout.

## How it runs

- **In-cluster it is NOT its own CronJob.** The hourly `batch-conductor` (helm `cronjobs.batch-conductor`, `10 * * * *`) runs the SAME `Verifier` as its final chain step (`pkg/batch/chain.go`, step `privacy-verify`, right after `privacy-delete`) and fails the step red on any incomplete deletion.
- This binary is the host-runnable escape hatch: `go run ./cmd/privacy-verify` (no Dockerfile in `build/`).

## Gotchas

- **RLS hatch is load-bearing:** `audience_segment_members` has RLS; the residual check runs inside one read-only tx with a tx-local `app.platform_read='on'` (`PostgresStore.Residual`). A bare-pool read under the NOBYPASSRLS `adtech_app` role sees 0 rows and would falsely certify a purge. Any new residual check must live inside that tx.
- **Verify list must track the purge list.** Every system the Deleter purges needs a paired residual check (`ExtraPurger` pairs `PurgeExtra`/`ResidualExtra`); a purged-but-unchecked system is an unverified purge.
- Idempotent: verified rows drop out of the query; incomplete rows are simply retried next run.
- No NATS — the Verifier publishes nothing (the Deleter announces completion on `events.SubjectPrivacyCompleted`, not this job).

## Core packages / config

- `pkg/privacydelete` — `Verifier`, `PostgresStore`, `BuildExtras`
- Config: nil service schema; reads `keys.Database.URL`, and extras read `keys.Reporting.ClickHouse*` + `keys.S3.*` + `keys.Pipeline.DatalakeBucket`

## Pointers

- `docs/PLAN.md` → "9a. User Opt-Out and Data Deletion System"
- e2e coverage: `tests/e2e/batch_conductor_test.go` asserts the `privacy-verify` chain step runs.
