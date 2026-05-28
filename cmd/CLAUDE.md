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

## Jobs (short-lived, K8s CronJobs or one-off)

| Job | What it does | Schedule |
|---|---|---|
| `seed/` | Loads seed data profiles into the database. | Manual (Tilt button) |
| `simulator/` | Generates fake ad traffic for testing. | Manual (Tilt button) |
| `migrate/` | Runs database migrations (goose). | Before every deploy |
| `rollup/` | Aggregates event data by time granularity. | Every minute/hour/day/month |
| `optimise/` | Runs optimisation pipelines (bid, placement, creative). | Hourly/daily |
| `fraud/` | Batch fraud detection and scoring. | Daily |
| `adstxt/` | Crawls and caches publisher ads.txt files. | Every 24 hours |

## Conventions

- Each service follows the same startup pattern: load config -> connect deps -> register health checks -> start server -> wait for shutdown signal
- Use `pkg/lifecycle/` for graceful shutdown
- Use `pkg/config/` for configuration loading (defaults -> env -> live config)
- Use `pkg/logger/` for structured logging
- Use `pkg/health/` for health check registration
- See each service's own CLAUDE.md for specific details

## Diagram Rule

**If your change affects how services connect (new gRPC call, new NATS subject, new dependency), you MUST update the relevant diagrams.** Each service's CLAUDE.md lists which diagrams to check. Run `make diagrams` to regenerate SVGs from D2 source files.
