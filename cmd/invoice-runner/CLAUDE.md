# invoice-runner Job

Generates advertiser invoices from billed spend: sums
`campaign_committed_spend.settled_micros` per campaign over a period (join via
`line_items` — `campaign_id` == `line_items.id`), converts micros → dollars, and
writes ONE `invoices` row + per-campaign `invoice_line_items` per account
(net-30 due date). Then, on the all-accounts pass only, writes seat-keyed
data-fee receivable drafts (`pkg/invoicing/datafee.go`).

## How it runs

- Monthly K8s CronJob `0 2 1 * *` — bills the calendar month that just closed;
  02:00 gives the day-boundary/rollup chain time to settle the month's final day
  (`k8s/helm/adtech/values.yaml`, `concurrencyPolicy: Forbid`, component `billing`).
- Also host-runnable one-off: `go run ./cmd/invoice-runner` (last month, all accounts);
  `--month 2026-06`, `--account <uuid>`, or `--period-start/--period-end` (end exclusive).
  Only env: `DATABASE_URL` (defaults to `routes.DefaultPostgresURL`).
- Core logic is `pkg/invoicing.Generator` (`generator.go`); the binary is a thin period-resolver + runner.

## Idempotency & gotchas

- Idempotent, keyed on `invoices` UNIQUE `(account_id, period_start, period_end)`
  (migration 051): a re-run UPDATEs the header total and REPLACES line items — never duplicates.
- Data-fee receivables are idempotent per (seat, period) too, but rows past `draft`
  (sent/paid/void) are NEVER rewritten.
- The all-accounts run bills only `payment_terms='invoiced'` accounts — prepay accounts
  already paid via topup and MUST NOT be billed again. `--account` bypasses that filter
  (deliberate staff override).
- No settled spend in the period → no invoice row (an empty invoice is noise, not a bill).
- RLS: per-account reads/writes set the tenant GUC in a tx; the cross-tenant account
  enumeration uses the `app.platform_read` hatch — a bare-pool query on these RLS tables
  under `adtech_app` silently returns 0 rows (i.e. invoices nobody).

## Pointers

- `docs/PLAN.md` -> "Billing and Financial Reconciliation" -> "Invoice and Payout Generation"
- Spend source of truth: `docs/PLAN.md` -> "Single Source of Truth: The AuctionWinEvent"
- Final-invoice-on-closure reuses the same Generator — see `cmd/account-closeout/CLAUDE.md`.
