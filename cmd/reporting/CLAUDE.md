# Reporting Service

Consumes events from NATS, writes to analytics store, serves query API for dashboards and custom reports.

## Responsibilities

- Consume events from NATS (impressions, clicks, conversions, views, auction events)
- Write to analytics store (DuckDB or ClickHouse)
- Serve query API via gRPC (metrics, trace events, custom report builder)
- Auto-select rollup tier based on query time range

## Key Packages Used

- `pkg/events/` - NATS consumption
- `pkg/store/analytics/` - DuckDB/ClickHouse interface
- `pkg/reporting/builder.go` - custom report query construction (multi-tenant filtered)
- `pkg/reporting/templates.go` - pre-built report definitions

## gRPC Services Exposed

- `ReportingService` - see `pkg/proto/`

## CRITICAL: DuckDB Constraints

When using DuckDB (local/staging):
- Single-writer only - max 1 replica
- Uses `build/Dockerfile.reporting` (CGO_ENABLED=1 for DuckDB driver)
- DuckDB file on PersistentVolumeClaim

When using ClickHouse (prod):
- Multiple replicas supported
- Uses shared `build/Dockerfile` (CGO_ENABLED=0)

## Dependencies

- NATS JetStream (consumes event streams)
- DuckDB or ClickHouse (analytics store)
- Postgres (saved reports, report scheduling config)

## Architecture Details

See `docs/PLAN.md` -> "Reporting and Analytics", "Data Rollups", "Custom Report Builder", "Billing and Financial Reconciliation"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New NATS subject consumed?** Update NATS Subjects table + NATS Event Flow diagram
- **New analytics table/column?** Update ER diagram + `migrations/ANALYTICS_SCHEMA.md`
- **New report type or metric?** Update relevant reporting metrics section in PLAN.md
- **Changed billing flow?** Update Billing Flow diagram in PLAN.md
