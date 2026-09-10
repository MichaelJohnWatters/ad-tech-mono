# payout-runner Job

Generates publisher payouts — the money-OUT mirror of `invoice-runner`. For each
active publisher it sums the period's gross revenue (winning `clearing_price_usd`)
from **ClickHouse** `impressions`, applies the publisher's revenue-share contract
(`pkg/billing.Contract.CalculateRevenue`) to get the net owed, gates on the
publisher's `payout_methods.minimum_payout_cents` threshold, and writes ONE
`payouts` row per (publisher, period) with `amount` (net), `platform_fee`
(margin), `status='pending'`.

Earnings use the SAME formula as the reporting `net_revenue` metric (gross ×
contract), so a payout matches what the publisher's earnings console shows. A
payout is a SETTLEMENT RECORD, not a ledger mutation — no balance is moved.

## How it runs

- Monthly K8s CronJob `0 3 1 * *` — pays out the calendar month that just closed;
  03:00 is AFTER `invoice-runner` (02:00) so settlement + invoicing have finished
  (`k8s/helm/adtech/values.yaml`, `concurrencyPolicy: Forbid`, component `billing`).
- Host-runnable one-off: `go run ./cmd/payout-runner` (last month, all publishers);
  `--month 2026-06`, `--publisher <uuid>`, or `--period-start/--period-end` (end
  exclusive). Env: `DATABASE_URL` (defaults to `routes.DefaultPostgresURL`),
  `CLICKHOUSE_ADDR`/`_USER`/`_PASSWORD`/`_DATABASE` (the earnings source; defaults
  mirror reporting's — `adtech`/`adtech-local-dev`, addr `127.0.0.1:9000`).
- Core logic is `pkg/payouts.Generator` (`generator.go`); the binary is a thin
  period-resolver + Postgres/ClickHouse wiring. Pure payout math is
  `payouts.ComputePayout` (unit-tested).

## Idempotency & gotchas

- Idempotent, keyed on `payouts` UNIQUE `(publisher_id, period_start, period_end)`
  (migration 101): a re-run UPSERTs `amount`/`platform_fee` ONLY while the payout
  is still `pending`; an already `processing`/`paid` payout is never rewritten.
- **Minimum threshold HOLDS, does not carry forward.** If net < the publisher's
  `minimum_payout_cents`, NO row is written and the earnings are held (a future
  publisher-balance table could accumulate them — deferred MVP limitation).
- Net rounds to CENTS. A single low-CPM impression can round to `$0.00` and write
  nothing — that's correct (there's nothing to pay).
- Guaranteed-minimum contracts: the per-impression floor is applied to aggregate
  gross (same approximation reporting's `net_revenue` makes); `platform_fee` goes
  NEGATIVE when the platform subsidizes to meet the floor.
- RLS: cross-publisher enumeration reads under the `app.platform_read` hatch; each
  payout WRITE sets the owning account's tenant GUC in a tx (payouts RLS policy,
  migration 017). Earnings come from ClickHouse, NOT the Postgres ledger.
- Account-closeout reuse: `Generator.GenerateForPublisher` is the single-publisher
  entry the closeout final-payout path can call (currently deferred — see
  `pkg/accountlifecycle/closeout.go`).
