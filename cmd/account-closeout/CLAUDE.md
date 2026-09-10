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
- **Publisher final payout is WIRED** (`pkg/payouts.Generator.GenerateForPublisher`):
  for each publisher entity the account owns, generates a payout over the un-paid
  tail (start = last `payouts.period_end` else account creation, so it never
  overlaps a monthly payout — no double-pay), end = start of tomorrow. Needs
  ClickHouse (the gross source) — `CLICKHOUSE_ADDR/_USER/_PASSWORD` env; if
  unreachable the run DEGRADES GRACEFULLY (advertiser closeouts still settle,
  publisher payouts are skipped + logged, not fatal).
- **The 90-day data purge is WIRED** (`accountlifecycle.Purger.RunDuePurges`, runs
  as a second pass after the closeouts): scans closures 'closed' for
  `RetentionDays` (90), then destructively deletes the account's **private**
  data across Postgres (the `purgeableTables` **allowlist** ∩ tables that carry an
  `account_id` — a destructive op FAILS SAFE, so a new table isn't swept in until
  someone adds it; FK order handled by retry passes; deletes scoped by the tenant
  GUC + explicit `WHERE account_id`) and ClickHouse
  (`analytics.ClickHouse.PurgeAccount`, `ALTER … DELETE` with `mutations_sync=1`),
  then flips the closure to the terminal `'purged'` state (`purged_at` set).
  **RETAINED** (NOT purged): financial/legal records (invoices, invoice_line_items,
  payouts, adjustments, topups, advertiser_balances, data_fee_earnings — tax
  retention), the `accounts` row + closure record + `audit_log` (tombstone),
  platform-global (`partners`), and cross-tenant-referenced tables
  (`marketplace_listings` — buyers' grants cascade off it — and `data_providers`).
  Idempotent (only `'closed'` selected) + best-effort per account. Skips the
  ClickHouse half (logs, retries next run) if CH is unreachable.
- **Object-storage purge** (`Purger.purgeObjects`, runs BEFORE the Postgres purge so
  the pointer rows are still present; aborts the whole purge on any failure so an
  account is never marked `'purged'` with a blob left behind): deletes the account's
  export ZIP (`account_export_jobs.artifact_bucket/key` — a full copy of the exported
  data) AND its transcoded SSAI creative segments (`{transcoder.prefix}/{creativeID}/…`
  in `s3.bucket`, keyed by the account's own UUID creative ids → tenant-safe). Needs
  `S3_ENDPOINT/S3_ACCESS_KEY/S3_SECRET_KEY` env (real S3 injected over these in
  staging/prod). NOT purged: shared/demo creative theme assets (`creatives.asset_url`
  → `themes/…`, reused across accounts) — there is no advertiser creative-upload path
  writing account-scoped blobs (a follow-up only if that feature lands).
  KNOWN GAP: a very large ClickHouse table's `mutations_sync=1` delete could exceed
  the purge's 30-min ctx (then it retries next run).

## Pointers

- `docs/PLAN.md` -> "Business Operations" -> "1. Account Closure and Data Export"
- Request/grace/export flow (migrations 091/092) lives in the gateway + report-runner; this job is only the settlement/close step.
