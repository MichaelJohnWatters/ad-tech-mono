# account-closeout Job

Finalizes account closures whose 30-day grace period has elapsed (PLAN Phase 11,
item 105): for each due closure it generates the final invoice (advertisers/
agencies), marks the account `closed`, and closes the request row.

## How it runs

- Daily K8s CronJob at 03:00 (`schedule: "0 3 * * *"`, after invoice-runner's
  02:00 slot) — `k8s/helm/adtech/values.yaml`, `concurrencyPolicy: Forbid`,
  component `billing`. Also host-runnable one-off: `go run ./cmd/account-closeout`
  (only env read: `DATABASE_URL`, defaults to `routes.DefaultPostgresURL`).
- Core logic is `pkg/accountlifecycle.CloseOut.RunDue` (`closeout.go`); final
  invoice via `pkg/invoicing.Generator.GenerateForAccount` (writes nothing when
  there's no un-billed settled spend — empty invoice id + nil error).

## Idempotency & gotchas

- Idempotent: each closure flips `grace → closed` exactly once (`WHERE status = 'grace'` guard); safe to re-run.
- Best-effort per account: one account's failure is logged (ERROR) and skipped, not fatal.
- Cross-tenant reads + the close writes run in a tx under the platform hatch
  (`app.platform_read` GUC) — a bare-pool query on these RLS tables under
  `adtech_app` silently returns 0 rows. The invoice write itself is tenant-scoped
  (invoicing sets `app.current_account_id`).
- Final-invoice window is DAY-bounded (committed spend is DATE-keyed): start = last
  `invoices.period_end` (else account creation day), end = start of tomorrow so today bills.
- Publisher final payout + the 90-day data purge are DEFERRED — `closed_at` is left set
  so the future purge job can scan `closed_at + accountlifecycle.RetentionDays <= now`.

## Pointers

- `docs/PLAN.md` -> "Business Operations" -> "1. Account Closure and Data Export"
- Request/grace/export flow (migrations 091/092) lives in the gateway + report-runner; this job is only the settlement/close step.
