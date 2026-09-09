# Batch Conductor (CronJob)

THE hourly data chain as ONE completion-ordered run (`pkg/batch.StandardChain`) — each step
starts when the previous actually finishes. Replaced the time-staggered CronJob lattice.

## The chain (order IS the dependency graph — see `pkg/batch/chain.go`)

1. `checkpoint` (CRITICAL) — pipeline + reporting `/readyz`; the only step that aborts the chain
2. `rollup:minute` → `hourly` → `daily` → `monthly` — reporting `GET /v1/reporting/rollup/run?level=X&lookback=…` (`routes.ReportingRollupRun`), finest first
3. `ch-parquet-export` — reporting `POST /debug/export/run` (`routes.ReportingExportRun`; ClickHouse→Parquet lake, ADR 0006; idempotent per hour — a retried chain re-exports harmlessly)
4. `profile-builder` — `pkg/profilebuilder.Run` in-process (behaviour reads via ClickHouse; lake-read fallback)
5. `privacy-delete` → `privacy-verify` — `pkg/privacydelete` in-process; verify AFTER delete

(compact/vacuum retired with the Delta dual-write — ADR 0006; the lake is a derived export now.)

## How it runs

- Helm CronJob `"10 * * * *"`, `concurrencyPolicy: Forbid`, `backoffLimit: 1`,
  `activeDeadlineSeconds: 1800` (matches the 30-min ctx timeout). One-shot; K8s owns the cadence.
- Every step writes a `batch_runs` row (staff portal → Batch runs); run announced on NATS
  `adtech.batch.run_completed` (`events.SubjectBatchRunCompleted`:
  {schema_version, run_id, aborted, failed_steps, duration_ms}).
- Exit 1 = CRITICAL step failed; exit 2 = privacy-verify found residual PII after a deletion.
  Non-critical failures continue the chain (stale, not wrong — every step is an idempotent
  wholesale-recompute).
- Config: `batch_conductor.*` keys (`pkg/config/keys/batchconductor.go`) — nats/pipeline/reporting
  URLs, datalake bucket (MUST match the pipeline's), clickhouse_*. Registers no schema
  (nil `config.Setup` — one-shot posture).

## Gotchas

- `BATCH_CONDUCTOR_CLICKHOUSE_ADDR` must point at the in-cluster ClickHouse — the
  `127.0.0.1:9000` default / an unreachable addr silently falls back to lake reads,
  re-opening the phase-2 OOM path (helm sets `clickhouse:9000`).
- Checkpoint approximates "ingestion caught up" via readiness, not consumer lag — safe only
  because downstream steps recompute wholesale; revisit if a step becomes lag-sensitive.
- Degrades, never blocks: missing NATS skips invalidates + the run announcement; missing S3
  limits profile-builder to clustering; no local S3 endpoint falls back to `/tmp/adtech-datalake`.

## Pointers

- `docs/PLAN.md` → "Data Rollups", "Universal Rollup Framework", "NATS Subjects" (the
  `adtech.batch.run_completed` row), "Profile Store (Normalized Signals → Expansion →
  Memberships → Export)"
- New NATS subject or chain step? Update the NATS Subjects table + check
  `docs/diagrams/README.md` "Update when" column, then `make diagrams`.
