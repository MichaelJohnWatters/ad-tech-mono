# Reporting Service

Consumes events from NATS, writes to analytics store, serves query API for dashboards and custom reports.

## Responsibilities

- Consume events from NATS (impressions, clicks, conversions, views, auction events)
- Write to the analytics store (ClickHouse; `memory` for unit tests)
- Serve query API via gRPC (metrics, trace events, custom report builder)
- Auto-select rollup tier based on query time range
- Own the hourly ClickHouse→Parquet export (the derived cold archive) + serve
  cold reads via ClickHouse `s3()` over it (ADR 0006)

## Key Packages Used

- `pkg/events/` - NATS consumption
- `pkg/store/analytics/` - analytics store interface (ClickHouse + memory) +
  HotColdStore (hot ClickHouse / cold `s3()` over the Parquet export)
- `pkg/reporting/builder.go` - custom report query construction (multi-tenant filtered)
- `pkg/reporting/templates.go` - pre-built report definitions

## gRPC Services Exposed

- `ReportingService` - see `pkg/proto/`

## Analytics store (ClickHouse)

ClickHouse is the single analytical store (ADR 0006 — DuckDB was retired). The
driver is pure Go, so reporting runs multi-replica on the standard
`build/Dockerfile.reporting` (CGO is only for tigerbeetle-go, the billing
ledger). Cold/deep-history reads are ClickHouse `s3()` over the hourly Parquet
export, not a separate engine. `reporting.analytics_backend` is `clickhouse`
(prod/full-local) or `memory` (volatile unit-test/CI default).

## Dependencies

- NATS JetStream (consumes event streams)
- ClickHouse (analytics store) + Minio/S3 (the Parquet export archive)
- Postgres (saved reports, report scheduling config)

## Architecture Details

See `docs/PLAN.md` -> "Reporting and Analytics", "Data Rollups", "Custom Report Builder", "Billing and Financial Reconciliation"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New NATS subject consumed?** Update NATS Subjects table + NATS Event Flow diagram
- **New analytics table/column?** Update ER diagram + `migrations/ANALYTICS_SCHEMA.md`
- **New report type or metric?** Update relevant reporting metrics section in PLAN.md
- **Changed billing flow?** Update Billing Flow diagram in PLAN.md
