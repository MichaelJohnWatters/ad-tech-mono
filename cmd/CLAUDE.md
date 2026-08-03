# cmd/ - Service Entrypoints

Each subdirectory is a separate Go binary deployed as a K8s service or job.

## Services (long-running)

| Service | What it does | Key dependencies |
|---|---|---|
| `dsp/` | Demand-side platform. Evaluates bid requests, manages campaigns/budgets, submits bids. | Postgres, Redis, NATS |
| `ssp/` | Supply-side platform. Manages publisher inventory, generates bid requests. | Postgres, Exchange (gRPC) |
| `exchange/` | Ad exchange. Runs auctions, fan-out to DSPs, deal priority. | NATS, DSP (OpenRTB), Ad Server (gRPC) |
| `adserver/` | Serves ad creatives to browsers, generates tracking URLs. | Object storage, Redis, Tracker (gRPC) |
| `tracker/` | Records impressions, clicks, conversions, viewability. Publishes to NATS. | NATS, Redis |
| `reporting/` | Consumes events from NATS, writes to analytics store, serves query API + owns the hourly ClickHouse→Parquet export. | NATS, ClickHouse, Minio/S3 |
| `gateway/` | API gateway. Auth, HTMX dashboard, REST API, proxies to internal gRPC services. | All services (gRPC) |
| `pipeline/` | Data pipeline. Ingests publisher files, validates, normalises, enriches; runs the audience drop-zone poller. (The Delta dual-write sink was retired in ADR 0006 — the lake is now a reporting-owned Parquet export.) | Object storage, Postgres, NATS |
| `billing/` | Billing service. Consumes AuctionWinEvents, accrues spend, generates invoices. | NATS, Postgres |
| `webhooks/` | Webhook dispatcher. Consumes NATS events, delivers HTTP POST to registered URLs. | NATS, Postgres |
| `notifications/` | In-app notification builder (:8096). Consumes the same account-scoped business events as webhooks (budget/balance depleted, campaign state changed, report completed) and writes one per-account row to `notifications` for the portal bell. Gateway serves list/unread/mark-read. Run 1 replica (queue-grouped). Core in `pkg/notifications`. | NATS, Postgres |
| `identity-consumer/` | Builds the identity graph. Consumes `adtech.identity.observed` from the SSP, batches/dedupes, writes edges (deterministic + probabilistic). Run 1 replica (in-memory fingerprint buckets). | NATS, Postgres |
| `audience-rt/` | Real-time retargeting (:8097). Consumes `adtech.behaviour.observed` (site_visit → enroll the visitor into the advertiser's retargeting segment) + `adtech.events.conversion` (purchase → suppress). Writes the same `audience_segment_members` the batch profile-builder would + broadcasts `cache.invalidate.audience`, so the DSP retargets within seconds instead of the hourly build. Single-visit rules (`min_count<=1`) only; frequency rules stay batch. Core in `pkg/retargeting`. Run 1 replica. | NATS, Postgres |
| `report-runner/` | Async report worker (:8095). Enqueues due saved-report schedules as report jobs, drains the `report_jobs` Postgres queue (SKIP LOCKED), renders CSV/JSON/Parquet artifacts into the private `adtech-reports` bucket, emails download links. Run 1 replica (boot-time stuck-job requeue). Core in `pkg/reportjobs` + `pkg/reportrunner`. | Postgres, Reporting (HTTP), Minio/S3, SMTP |

## Host tools (never deployed)

| Tool | What it does |
|---|---|
| `devconsole/` | Host dev-loop UI at localhost:8099 (`make devconsole`): build/deploy buttons streaming `make deploy SVC=x` output, stack-up/seed/demo shortcuts, links to portals/observability. Host-only because builds need the local toolchain + docker socket; cluster-side ops live in the staff portal's Ops section. |

## Jobs (short-lived, K8s CronJobs or one-off)

| Job | What it does | Schedule |
|---|---|---|
| `seed/` | Loads seed data profiles into the database. | Manual (Tilt button) |
| `simulator/` | Generates fake ad traffic for testing. | Manual (Tilt button) |
| `migrate/` | Runs database migrations (goose). | Before every deploy |
| `batch-conductor/` | THE data chain, completion-ordered (pkg/batch): checkpoint → rollup tiers → ch-parquet-export (ClickHouse→Parquet, via reporting HTTP) → profile-builder → privacy delete → verify. Replaced the time-staggered lattice; steps recorded in `batch_runs`. (compact/vacuum retired with the Delta dual-write, ADR 0006.) | Hourly |
| `profile-builder/` | Profile store expansion engine (pkg/profilebuilder) — standalone escape hatch; the conductor runs it as a chain step. | Via conductor (or manual) |
| `optimise/` | Runs optimisation pipelines (bid, placement, creative). | Hourly/daily |
| `fraud/` | Batch fraud detection and scoring. | Daily |
| `adstxt/` | Crawls and caches publisher ads.txt files; publishes the ads-txt cache invalidate on change. | Every 24 hours |
| `invoice-runner/` | Generates advertiser invoices from billed spend (pkg/invoicing): sums `campaign_committed_spend.settled_micros` per campaign over a period, converts micros→dollars, writes one `invoices` row + per-campaign `invoice_line_items` per account. Idempotent (keyed on account+period). Host-runnable one-off (`go run ./cmd/invoice-runner`; `--account`, `--month`, `--period-start/--period-end` overrides; defaults to last calendar month, all accounts). | Monthly (02:00 on the 1st) |

## Conventions

- Each service follows the same startup pattern: `config.Setup() -> connect deps -> register health checks -> start server -> wait for shutdown signal`
- **MUST use shared packages** - never hardcode values that exist in a shared package:
  - `pkg/config/` - `config.Setup(serviceName, keys.<Svc>Schema(), log)` for live config with polling; read keys via typed handles (`keys.<Svc>.X.Get(cfg)`) declared once in `pkg/config/keys/` — never string literals
  - `pkg/constants/` - service names, status values, bid models, device types, channels
  - `pkg/routes/` - all HTTP paths, ports, service URLs (versioned with `routes.APIVersion`)
  - `pkg/events/` - NATS subject constants, event payload types, publisher helper
  - `pkg/logger/` - `logger.New(constants.ServiceXxx)` for structured JSON logging
  - `pkg/health/` - `/healthz` and `/readyz` endpoints
  - `pkg/lifecycle/` - graceful shutdown with `lifecycle.ServeHTTP()`
  - `pkg/middleware/` - CORS, auth (JWT/RBAC), metrics, reverse proxy
- **Never hardcode** port numbers, service URLs, route paths, status strings, or NATS subjects
- See each service's own CLAUDE.md for specific details

## Diagram Rule

**If your change affects how services connect (new gRPC call, new NATS subject, new dependency), you MUST update the relevant diagrams.** Each service's CLAUDE.md lists which diagrams to check. Run `make diagrams` to regenerate SVGs from D2 source files.
