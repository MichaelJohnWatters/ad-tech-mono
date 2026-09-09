# pkg/store - Database Access Layer

Umbrella for every persistence backend. No Go code at this root — each subpackage
defines its interface at its own root with implementations alongside/in subdirs.

## Subpackages & Entry Points

| Subpackage | What | Start from |
|---|---|---|
| `postgres/` | Transactional store, multi-tenant via RLS | `postgres.New` / `NewFromDB` (`postgres.go`); domain methods per file (`campaigns.go`, `deals.go`, …) |
| `analytics/` | Event store interface: `MemoryStore`, `ClickHouse` (native protocol, `PrepareBatch` bulk path) | `analytics.Store` (`analytics.go`); optional capabilities discovered by type assertion — `ObservabilityWriter`, `RollupWriter`, `ViewThroughReader`, `AttributionWriter/Reader`, `TraceReader`, `ParquetExporter` (`export.go`) |
| `analytics/` (hot/cold) | `HotColdStore` splits reads at `reporting.hot_window`: recent → ClickHouse, older → `ColdReader`; `CHParquetColdReader` = CH `s3()` over the Parquet export (ADR 0006) | `hotcold.go`, `coldreader_s3.go`; gated by `reporting.cold_store_enabled` |
| `objects/` | Object storage: `s3/` (Minio + real S3) and `fs/` (tests/fallback) | `objects.Store` (`objects.go`); `objects.Connect` picks backend from `s3.endpoint` |
| `datalake/` | Parquet + Delta Log on object storage (ACID append, `PurgeRows` for GDPR) | `datalake.Store` (`datalake.go`) |
| `rollup/` | Config-driven aggregation engine, raw → minute → hourly → daily → monthly | `rollup.NewEngine` (`rollup.go`); `StandardRetention` |

## CRITICAL: Postgres tenancy (RLS)

- App services connect as the NOBYPASSRLS role `adtech_app` (mig 067). Under it,
  **a bare-pool query on an RLS table silently returns/matches 0 rows** — no
  error. Every query needs the tenant GUC or the platform hatch:
  - **Tenant-scoped**: `Store.WithTx` (sets `app.current_account_id` via
    `SetTenantContext`), or on a raw `*sql.DB` the helpers
    `QueryTenantDB` / `QueryRowTenantDB` / `ExecTenantDB`.
  - **Deliberately cross-tenant** (warm-cache LoadAll loaders, background
    sweeps): `Store.QueryPlatform` / `QueryRowPlatform` / `ExecPlatform` —
    the `app.platform_read` hatch (mig 065). Justify every use.
- Account ID rides context: `postgres.WithAccountID` / `AccountIDFromContext`.
  RLS is the net; methods must still filter by `account_id` explicitly.
- `QueryPlatform`/`QueryTenantDB` hold the tx OPEN until `closeFn` — `defer` it
  and finish scanning first (lib/pq invalidates `*sql.Rows` when the tx ends;
  committing before returning rows silently yields ZERO rows).
- Pooled connections can leave the GUC as EMPTY STRING (not unset) after a
  tenant tx; a policy that casts it `::UUID` bare errors 22P02. New RLS policies
  use the `NULLIF(current_setting(...), '')::UUID` shape (mig 087).
- `New` calls `LogRLSEnforcement` at boot — a superuser/BYPASSRLS role logs a
  loud ERROR (isolation silently disabled). Never downgrade that to info.
- New tables need an RLS policy — see `migrations/CLAUDE.md`.

## Other invariants

- `HotColdStore` WRITES always go hot; only reads split. Tenant filters in
  `QueryParams.Filters` ride through both halves unchanged.
- New optional capability on `ClickHouse`? `HotColdStore` MUST forward it (or
  callers reach it via `HotStore()`) — type assertions on the wrapped store
  silently fail once `reporting.cold_store_enabled` swaps the wrapper in
  (`hotcold.go` lines ~121–130).
- `objects.Connect` never latches the fs fallback when `s3.endpoint` is set: it
  serves fs while retrying the S3 dial in the background and swaps in the real
  backend when it lands (boot-latch doctrine — multi-replica fs = split-brain
  emptyDirs; fallback-era writes stay on the pod's local disk).
- `CampaignLoader` (postgres) is cross-tenant BY DESIGN for the DSP warm cache;
  request-time handlers must never reuse it.
- Stale-doc warning: `analytics.go` package comments and the
  `reporting.cold_store_enabled`/`hot_window` key prose still mention DuckDB;
  the DuckDB store was deleted in ADR 0006 — cold is `CHParquetColdReader`.

## Used by

Nearly every service. `postgres/`: gateway, dsp, ssp, exchange, adserver,
publisher-adserver, tracker, reporting, pipeline, identity-consumer, dayboundary.
`analytics/`: reporting, tracker, gateway, audience-rt.
`objects/`: adserver, pipeline, gateway, reporting, report-runner, seed, ssai,
content-packager (+ transcoder via `pkg/transcode`, batch-conductor /
profile-builder via `datalake`).
`datalake/`: batch-conductor, profile-builder, pipeline, gateway.
`rollup/`: reporting.

## Testing

- Unit: `analytics.MemoryStore` and `objects/fs` are the in-package fakes; never
  mock Postgres/Redis (root CLAUDE.md).
- Integration: `-tags=integration` (`rls_platform_read_integration_test.go` uses
  real Postgres); ClickHouse behind `-tags=clickhouse_integration`.

## Pointers

- `docs/PLAN.md` → "Multi-Tenancy Isolation" (RLS layers), "Data Storage",
  "Data Rollups" / "Universal Rollup Framework"
- `docs/adr/0006-clickhouse-primary-parquet-export.md` (hot/cold spine)
