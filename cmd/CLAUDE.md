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
| `reporting/` | Consumes events from NATS, writes to analytics store, serves query API. | NATS, DuckDB/ClickHouse |
| `gateway/` | API gateway. Auth, HTMX dashboard, REST API, proxies to internal gRPC services. | All services (gRPC) |
| `pipeline/` | Data pipeline. Ingests publisher files, validates, normalises, enriches. | Object storage, Postgres, DuckDB |
| `billing/` | Billing service. Consumes AuctionWinEvents, accrues spend, generates invoices. | NATS, Postgres |
| `webhooks/` | Webhook dispatcher. Consumes NATS events, delivers HTTP POST to registered URLs. | NATS, Postgres |
| `identity-consumer/` | Builds the identity graph. Consumes `adtech.identity.observed` from the SSP, batches/dedupes, writes edges (deterministic + probabilistic). Run 1 replica (in-memory fingerprint buckets). | NATS, Postgres |
| `report-runner/` | Async report worker (:8095). Enqueues due saved-report schedules as report jobs, drains the `report_jobs` Postgres queue (SKIP LOCKED), renders CSV/JSON/Parquet artifacts into the private `adtech-reports` bucket, emails download links. Run 1 replica (boot-time stuck-job requeue). Core in `pkg/reportjobs` + `pkg/reportrunner`. | Postgres, Reporting (HTTP), Minio/S3, SMTP |

## Jobs (short-lived, K8s CronJobs or one-off)

| Job | What it does | Schedule |
|---|---|---|
| `seed/` | Loads seed data profiles into the database. | Manual (Tilt button) |
| `simulator/` | Generates fake ad traffic for testing. | Manual (Tilt button) |
| `migrate/` | Runs database migrations (goose). | Before every deploy |
| `batch-conductor/` | THE data chain, completion-ordered (pkg/batch): checkpoint → compact (via pipeline HTTP — single-writer rule) → rollup tiers → profile-builder → privacy delete → verify. Replaced the time-staggered lattice (standalone compact/rollup/privacy crons); steps recorded in `batch_runs`. | Hourly |
| `profile-builder/` | Profile store expansion engine (pkg/profilebuilder) — standalone escape hatch; the conductor runs it as a chain step. | Via conductor (or manual) |
| `optimise/` | Runs optimisation pipelines (bid, placement, creative). | Hourly/daily |
| `fraud/` | Batch fraud detection and scoring. | Daily |
| `adstxt/` | Crawls and caches publisher ads.txt files; publishes the ads-txt cache invalidate on change. | Every 24 hours |

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
