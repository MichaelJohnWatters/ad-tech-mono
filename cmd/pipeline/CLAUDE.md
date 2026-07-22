# Pipeline Service

Ingests publisher data files in any format, validates, normalises to common schema, and enriches.

## Responsibilities

- Accept files in any format (CSV, TSV, JSON, Excel, Parquet)
- Auto-detect format, delimiter, encoding
- Validate against publisher-specific config (`profiles/publishers/*.yaml`)
- Quarantine bad rows/files with error details
- Normalise to common schema (field mapping, type casting, date parsing, dedup)
- Enrich with derived data (geo from IP, device classification, audience matching)
- Detect schema drift and alert
- Publish normalised/enriched events to NATS (reporting lands them in ClickHouse;
  the old Parquet+Delta dual-write was retired in ADR 0006)

## Key Packages Used

- `pkg/pipeline/` - format detection, validation, normalisation, enrichment logic
- `pkg/store/datalake/` - Parquet read/write, Delta Log management
- `pkg/store/objects/` - read source files from filesystem/S3
- `pkg/events/` - publish pipeline events to NATS (file ingested, quarantined, drift detected)

## gRPC Services Exposed

- `PipelineService` - see `pkg/proto/`

## Pipeline Stages

See `docs/PLAN.md` -> "Data Pipeline" for full stage details.

## Publisher Configs

Per-publisher YAML in `profiles/publishers/`. Defines field mapping, validation rules, expected format. See `docs/PLAN.md` -> "Publisher Configuration", "Schema Drift Detection"

## Dependencies

- Object storage (source files - filesystem/S3)
- Postgres (publisher configs, quarantine records)
- ClickHouse (analytical store — reporting consumes the events; the pipeline's
  own Delta dual-write was retired in ADR 0006)
- NATS (publishes pipeline events)

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New pipeline stage?** Update `docs/PLAN.md` -> Data Pipeline stages
- **New NATS subject published?** Update NATS Subjects table + NATS Event Flow diagram
- **New file format supported?** Update Pipeline section in PLAN.md
- **New dependency?** Update `docs/diagrams/architecture.d2`
