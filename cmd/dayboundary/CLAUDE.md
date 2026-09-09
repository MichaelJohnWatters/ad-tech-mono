# dayboundary Job

Daily K8s CronJob (helm `cronjobs.dayboundary`, `5 0 * * *` UTC, `concurrencyPolicy: Forbid`)
that processes campaign flight-date transitions at the day boundary. Phase 1 of
ADR 0005 §2 only: IO flight transitions with an explicit line-item cascade.

## What it does

- **Activate**: IOs `draft → active` where the flight window includes the day; cascades their
  `approved` line items → `live`. Flight dates live on the IO (line items have none); the bid
  path gates on line-item status, so without the cascade nothing starts bidding.
- **End**: IOs `active → ended` where `end_date` passed; cascades `live`/`paused` line items
  → `ended` (no FK cascade — explicit, else bids continue after the flight).
- Publishes one `adtech.campaign.state_changed` (CampaignStateEvent, reason
  `flight_start`/`flight_end`) per cascaded line item, plus a single
  `adtech.cache.invalidate.campaigns` if anything changed, so the DSP warm cache reloads
  immediately instead of waiting for its poll.

## How it runs

- `go run ./cmd/dayboundary` (today UTC) or `--date 2024-06-15`. Env: `DATABASE_URL`
  (required — exits without it), `NATS_URL` (optional).
- **Idempotent** — status predicates converge; safe to re-run for the same date.
- **NATS is best-effort**: DB transitions still commit without it; caches pick the change up
  on their next poll. Don't make event publish a hard dependency.

## Core packages

- `pkg/store/postgres/dayboundary.go` — `ActivateFlights`/`EndFlights` (single tx, captures
  old→new statuses for accurate state events). `main.go` orchestrates via the small
  `flightStore` interface so counts + publishing are unit-testable with a fake (`main_test.go`).
- `pkg/events` — `Publisher.CampaignStateChanged` + subject constants.

## Gotchas

- **LATENT RLS SILENT NO-OP (verify before trusting a run):** `transitionIOFlights` queries
  `insertion_orders`/`line_items` in a bare tx — no `app.platform_read` hatch, no tenant GUC —
  but both tables carry `tenant_isolation` RLS (mig 017/065) and the helm `DATABASE_URL`
  connects as `adtech_app` (`NOBYPASSRLS`, mig 067). Under that role the IO select sees 0 rows
  and the job no-ops while logging success. The in-code comment ("relies on the connecting
  role bypassing RLS") predates the role flip. Fix shape: `ExecPlatform`-style
  `set_config('app.platform_read','on',true)` inside the tx (cross-tenant background job —
  the documented legit use).
- Phases 2–4 (daily budget reset + snapshot, IO budget depletion, per-line-item timezone
  boundaries) are **not built** — see `docs/adr/0005-deferred-followups.md` §2. PLAN.md's
  `cmd/reporting --mode=day-boundary` + hourly-timezone design describes the target state,
  not this binary.

## Pointers

- `docs/PLAN.md` → "Campaign Flight Management" ("Day Boundary Processing",
  "Timezone-Aware Daily Budgets", "What Happens If the Day Boundary Job Fails")
- `docs/adr/0005-deferred-followups.md` §2 — the phased plan this implements
