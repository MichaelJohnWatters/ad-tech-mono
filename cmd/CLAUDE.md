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
| `billing/` | Shell — no Go code here. The billing engine (`pkg/billing`: AuctionWinEvent-sourced spend, reserve/settle, ledger) is hosted in-process by `cmd/reporting`. See `cmd/billing/CLAUDE.md`. | (see reporting) |
| `ssai/` | SSAI stitcher (:8093). Splices auction-won ads into HLS/DASH manifests server-side, HMAC-signed beacons on segment fetch; freq-cap PEEK/RECORD split. | Object storage, Redis, Transcoder |
| `transcoder/` | SSAI ad-conditioning (:8094). Cache-first ffmpeg transcode/segment of winning video/audio ads to content-compatible HLS in S3. | Object storage, Redis |
| `publisher-adserver/` | Publisher-side ad serving (:8088). Arbitrates direct-sold line items (sponsorship/guaranteed/house) vs the programmatic SSP auction + Prebid Server fan-out; Redis-shared delivery pacing. Core in `pkg/publisheradserver`. | Postgres, Redis, SSP, NATS |
| `webhooks/` | Webhook dispatcher. Consumes NATS events, delivers HTTP POST to registered URLs. | NATS, Postgres |
| `notifications/` | In-app notification builder (:8096). Consumes the same account-scoped business events as webhooks (budget/balance depleted, campaign state changed, report completed) and writes one per-account row to `notifications` for the portal bell. Gateway serves list/unread/mark-read. Run 1 replica (queue-grouped). Core in `pkg/notifications`. | NATS, Postgres |
| `identity-consumer/` | Builds the identity graph. Consumes `adtech.identity.observed` from the SSP, batches/dedupes, writes edges (deterministic + probabilistic). Multi-replica safe when the fingerprint buckets are Redis-backed (`IDENTITY_CONSUMER_REDIS_URL`; helm runs 3); 1 replica ONLY in the in-memory fallback. | NATS, Postgres, Redis |
| `audience-rt/` | Real-time retargeting (:8097). Consumes `adtech.behaviour.observed` (site_visit → enroll the visitor into the advertiser's retargeting segment) + `adtech.events.conversion` (purchase → suppress). Writes the same `audience_segment_members` the batch profile-builder would; the migration-078 changelog trigger + the pipeline's single drainer carry the write to Redis ≤3s (no invalidate broadcasts), so the DSP retargets within seconds instead of the hourly build. Single-visit rules (`min_count<=1`) only; frequency rules stay batch. Core in `pkg/retargeting`; see docs/AUDIENCE-PIPELINE.md. Multi-replica safe (idempotent upsert + queue-group consumption; helm runs 2). | NATS, Postgres |
| `report-runner/` | Async report worker (:8095). Enqueues due saved-report schedules as report jobs, drains the `report_jobs` Postgres queue (SKIP LOCKED), renders CSV/JSON/Parquet artifacts into the private `adtech-reports` bucket, emails download links; also drains account-closure export zips. Multi-replica via job leases (SKIP LOCKED + lease; helm runs 3), with a stale-lease sweep. Core in `pkg/reportjobs` + `pkg/reportrunner`. | Postgres, Reporting (HTTP), Minio/S3, SMTP |

## Host tools (never deployed)

| Tool | What it does |
|---|---|
| `devconsole/` | Host dev-loop UI at localhost:8099 (`make devconsole`): build/deploy buttons streaming `make deploy SVC=x` output, stack-up/seed/demo shortcuts, links to portals/observability. Host-only because builds need the local toolchain + docker socket; cluster-side ops live in the staff portal's Ops section. |
| `simulator/` | Host CLI generating production-like traffic (persona × channel, web-mirror or direct-OpenRTB, signed beacons, synthesised conversions, `--verify` reporting-invariant gate). Driven via `make traffic` / `make loadtest` / `make demo`. |
| `extbidder/` | External DSP simulator (:9100). Answers the exchange's fan-out over OpenRTB HTTP only (never internal gRPC); segtax audience uplift + debug seen-ring. |
| `demosite/` | External demo publisher site (:9000). Embeds the real adtech.js SDK/VAST tags — exercises the true cross-origin serving path. |
| `demoadv/` | External demo advertiser shop (:9200). Fires the real retargeting pixel + signed `/v1/t/conv` postback — the landing/conversion side of the CPA money loop. |
| `taxonomy-import/` | One-off importer: upserts the official IAB Audience Taxonomy TSV into `iab_audience_taxonomy` (replaces the migration-062 demo subset); idempotent, FK-safe `--prune`. |
| `advertisersmoke/` | Headless-Chrome smoke driving demoadv through consent + convert(); `scripts/advertiser-smoke.sh` asserts rows land in ClickHouse. |
| `viewabilitysmoke/` | Headless-Chrome smoke letting the demosite video page self-measure IAB viewability and fire the signed beacon; `scripts/viewability-smoke.sh` asserts the row lands in ClickHouse. |

## Jobs (short-lived, K8s CronJobs or one-off)

| Job | What it does | Schedule |
|---|---|---|
| `seed/` | Idempotent seed job: UPSERTs YAML profiles + feature baseline + dev logins/balances into Postgres (deterministic `pkg/idgen` IDs), uploads themed creatives to Minio/S3. | Manual (`make seed` / gateway `/dev/reset-and-reseed`) |
| `migrate/` | Runs database migrations (goose). | Helm post-install/upgrade hook (every deploy) |
| `batch-conductor/` | THE data chain, completion-ordered (pkg/batch): checkpoint → rollup tiers → ch-parquet-export (ClickHouse→Parquet, via reporting HTTP) → profile-builder → privacy delete → verify. Replaced the time-staggered lattice; steps recorded in `batch_runs`. (compact/vacuum retired with the Delta dual-write, ADR 0006.) | Hourly (`10 * * * *`) |
| `profile-builder/` | Profile store expansion engine (pkg/profilebuilder) — standalone escape hatch; the conductor runs it as a chain step. | Via conductor (or manual) |
| `dayboundary/` | IO flight-date transitions (draft→active / active→ended) with line-item cascade; publishes campaign state events + cache invalidate. Idempotent. | Daily (`5 0 * * *`) |
| `account-closeout/` | Finalizes 30-day-grace account closures: final day-bounded invoice, flips account + closure request to closed. Idempotent, best-effort per account. | Daily (`0 3 * * *`) |
| `invoice-runner/` | Generates advertiser invoices from billed spend (pkg/invoicing): sums `campaign_committed_spend.settled_micros` per campaign over a period, converts micros→dollars, writes one `invoices` row + per-campaign `invoice_line_items` per account. Idempotent (keyed on account+period). Host-runnable one-off (`go run ./cmd/invoice-runner`; `--account`, `--month`, `--period-start/--period-end` overrides; defaults to last calendar month, all accounts). | Monthly (`0 2 1 * *`) |
| `payout-runner/` | Generates publisher payouts (pkg/payouts) — the money-OUT mirror. Sums each publisher's gross revenue (`clearing_price_usd`) from **ClickHouse** over a period, applies their rev-share contract (`pkg/billing`) → net, gates on `payout_methods.minimum_payout_cents`, writes one idempotent `payouts` row per (publisher, period). Host-runnable (`go run ./cmd/payout-runner`; `--publisher`, `--month`, `--period-start/--period-end`); needs `CLICKHOUSE_ADDR/_USER/_PASSWORD` (earnings source) + `DATABASE_URL`. | Monthly (`0 3 1 * *`, after invoice-runner) |
| `privacy-delete/` | GDPR Level-3 purge executor: drains pending `opt_out_registry`, deletes from identity_graph + audience_segment_members (RLS platform hatch) + ClickHouse w/ Parquet re-export. | Via conductor chain step (binary = manual escape hatch) |
| `privacy-verify/` | GDPR deletion auditor: residual-checks completed-but-unverified users across Postgres + ClickHouse; stamps `verified_at`, exits 2 on residual PII. | Via conductor chain step (binary = manual escape hatch) |
| `adstxt/` | One-shot ads.txt crawler → `ads_txt_cache`; publishes the ads-txt cache invalidate on change. No Helm CronJob wired yet. | Manual one-shot |
| `appadstxt/` | app-ads.txt twin: crawls `{developer_domain}/app-ads.txt` per app publisher into `app_ads_txt_cache`. No NATS publish or enforcement consumer yet. | Manual one-shot |
| `content-packager/` | One-shot ffmpeg job packaging a source MP4 into a segmented HLS/CMAF VOD origin (stamped CUE-OUT/CUE-IN ad break) for the SSAI stitcher. | Manual one-shot |
| `prewarm/` | One-shot: pre-conditions every video/audio creative across the ABR ladder via the transcoder so SSAI hits a warm cache. | Manual one-shot |
| `optimise/` | PLACEHOLDER (.gitkeep only — PLAN step 63 closed "no binary needed"): the engine lives in `pkg/optimise`, consumed in-process by adserver (creative bandit) + exchange (SmartRouter). | — |
| `fraud/` | PLACEHOLDER (.gitkeep only — planned batch scoring CronJob, PLAN step 58): real-time fraud lives in `pkg/fraud` on the tracker path. | — |
| `cleanroom/` | PLACEHOLDER (.gitkeep only — planned isolated clean-room compute Job): shipped clean-room-lite lives in gateway + `pkg/marketplace`. | — |
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
