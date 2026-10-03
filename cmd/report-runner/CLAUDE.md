# Report Runner Service

Async report worker (:8095). Turns saved-report schedules and gateway-submitted
report jobs into rendered artifacts (CSV/JSON/Parquet) in the private
`adtech-reports` bucket, with email/webhook delivery. Also drains the account
data-export queue (PLAN Phase 11 #105). Off the serving hot path entirely.

## Responsibilities

Three ticker loops plus a shared queue (see the package comment in `main.go`):

- **Scheduler tick** (`report_runner.schedule_interval`, 60s) — enqueues due
  saved reports (@hourly/@daily/@weekly/@monthly) as `report_jobs` rows via
  `pkg/reportrunner.Runner`. Tenant scope filters are resolved at enqueue time
  (`reportjobs.ResolveTenantFilters`); the executor trusts the snapshotted filters.
- **Executor tick** (`report_runner.poll_interval`, 5s; drains back-to-back) —
  claims queued jobs (Postgres `FOR UPDATE SKIP LOCKED` + lease), runs the query
  against reporting over HTTP (`report_runner.query_timeout`, 10m), renders the
  artifact into `report_runner.artifact_bucket`, emails a download link
  (delivery=email) and/or publishes `report.completed` (delivery=webhook).
  Segment exports read `audience_segment_members` directly, not the query API.
  The same tick drains `account_export_jobs` (`pkg/accountexport.Worker`) —
  per-account data-export zips into the same bucket.
- **Sweep tick** (`report_runner.sweep_interval`, 1h) — deletes artifacts + job
  rows past `report_runner.retention` (720h).

Jobs also arrive from the gateway (`POST /v1/api/reports/jobs`,
`routes.APIReportJobs`) — the `report_jobs` table is the queue, whoever enqueues.

## Interfaces

- HTTP: only `/healthz`, `/readyz`, `/metrics` — no business endpoints; report
  downloads stream through the gateway after auth, never from here.
- NATS published: `adtech.report.completed` (SubjectReportCompleted → webhooks
  dispatcher). Optional: NATS down = warn, jobs still complete + links still work.
- NATS consumed: none.

## Key Packages Used

- `pkg/reportjobs/` - job queue store (SKIP LOCKED + lease), executor, formats, sweeper
- `pkg/reportrunner/` - saved-report schedule store + due-report runner, reporting HTTP query
- `pkg/accountexport/` - account-closure data-export worker + zip builder
- `pkg/email/` - SMTP (Mailpit/SES) when `report_runner.smtp_host` set, else memory sender that only logs
- `pkg/store/objects/` - Minio/S3 artifact store (FS fallback `/tmp/adtech-reports`)

## Dependencies

- Postgres (report_jobs queue, saved_reports schedules, account_export_jobs)
- Reporting service over HTTP (`report_runner.reporting_url`) — queries run there
- Minio/S3 (`adtech-reports` bucket, PRIVATE)
- SMTP + NATS (both optional/fail-soft)

## CRITICAL

- **Multi-replica safe** — claims carry a heartbeated lease; `ReclaimExpired`
  (boot + every executor tick) requeues only LAPSED leases, so a booting pod
  can't steal a live peer's job (the old started_at boot requeue could — that's
  what pinned this to 1 replica; local chart now runs `replicas: 3`). Stale
  "single-replica by design" comments linger in values-staging/prod + `cmd/CLAUDE.md`.
- **RLS discipline**: `report_jobs`/`account_export_jobs` have RLS. Worker-side
  cross-tenant claim/sweep runs under the platform hatch (`app.platform_read`);
  per-job reads (segment members, export builder, tenant list/get) set the job's
  `app.current_account_id` GUC in a tx. A bare-pool query here silently returns
  0 rows — never add one.
- **Bucket is PRIVATE** — never `SetPublicRead`; downloads are gateway-authed.
- **Boot-retry doctrine**: `EnsureBucket` failure retries in the background
  (never latch — a Minio boot race once failed every job until pod restart).

## Architecture Details

See `docs/PLAN.md` -> "Custom Report Builder" (save & schedule), "1. Account
Closure and Data Export" (Phase 11).

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New NATS subject published?** Update `docs/PLAN.md` -> NATS Subjects table + NATS Event Flow diagram
- **New dependency (e.g. a new store or service call)?** Update `docs/diagrams/architecture.d2` and run `make diagrams`
- **New job source/delivery mode?** Update `docs/PLAN.md` -> Custom Report Builder
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
