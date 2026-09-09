# Fraud Batch Job (NOT BUILT — placeholder)

Planned daily CronJob for batch (cold-path) fraud detection and scoring — PLAN
step 58, the outstanding item in Phase 6. **This directory is empty (`.gitkeep`
only): no binary, no Dockerfile in `build/`, no Helm CronJob.** Don't assume a
batch sweep runs; today only real-time fraud is live.

## Why it's not built

Blocked on data: the ClickHouse `impressions`/`clicks` tables carry **no IP
column** (only geo/device — see the schema in
`pkg/store/analytics/clickhouse.go`), so a velocity/suspicious-IP sweep has
nothing to aggregate. Sequence when building: (1) add IP capture to the event
schema, (2) this job scores historical events and writes `fraud_blocklists` —
the tracker's warm blocklist cache already consumes those rows, so enforcement
is wired the moment rows appear.
See `docs/PLAN.md` -> "Build Status & Outstanding Work" ledger, step 58.

## Where fraud lives TODAY (don't duplicate it here)

- `pkg/fraud/realtime.go` — hot-path checks (bot UA, IP blocklist, rate limit); `RealTimeChecker` runs in the **tracker only** (`cmd/tracker/main.go` + `blocklist.go` warm cache)
- `pkg/fraud/scoring.go` — weighted fraud score model (`Scorer`); **currently has no callers in `cmd/`** — it's the model this job would wrap
- `pkg/fraud/adstxt.go` / `appads.go` — seller authorisation; the exchange's `AdsTxtCache` gate + `cmd/adstxt/` / `cmd/appadstxt/` crawler CronJobs; `cmd/gateway/sellers.go` — sellers.json
- `pkg/store/postgres/fraud_blocklists.go` — blocklist store (migration 016; platform-global, no `account_id`, unique on type+value)
- Staff blocklist API `routes.APIFraudBlocklists` (`/v1/api/fraud/blocklists`, JWT `fraud:*`); create/delete publishes `events.SubjectCacheInvalidateFraudRules` (`adtech.cache.invalidate.fraud-rules`)

## When you build it

- Wrap `pkg/fraud` — detection logic belongs in the package, not this binary
- Idempotent runs (re-running a window must not double-flag or double-credit)
- Follow the job conventions in `cmd/CLAUDE.md` (config via `pkg/config` + typed keys, `pkg/logger`, no hardcoded subjects/paths/ports)
- Billing impact thresholds and the score model: `docs/PLAN.md` -> "Fraud Detection and Traffic Quality" ("Batch Detection (Cold Path)", "Fraud Scoring", "Billing Impact")

## Diagram Updates

Adding this CronJob = a new service connection (analytics reads,
`fraud_blocklists` writes, cache-invalidate publish): update the matching
diagram per `docs/diagrams/README.md` in the same PR, then `make diagrams`.
