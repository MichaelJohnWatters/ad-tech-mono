# Pipeline Service

Ingests publisher data files in any format, validates, normalises to common schema, and enriches.

## Responsibilities

- Accept files in any format (CSV, TSV, JSON, Excel, Parquet)
- Auto-detect format, delimiter, encoding
- Validate required fields per row (from the manifest/publisher config); **row-level**,
  not whole-file — a row missing/empty `id_value` is quarantined, good rows proceed
- Quarantine bad rows with error details to a `rejected/…` artifact (a file that yields
  zero valid rows, or is undecodable, is recorded terminal with 0 members)
- Normalise to common schema (field mapping, type casting, date parsing, dedup)
- Enrich with derived data (geo from IP, device classification, audience matching)
- Publish normalised/enriched events to NATS (reporting lands them in ClickHouse;
  the old Parquet+Delta dual-write was retired in ADR 0006)

> **Not built (planned — see `docs/PLAN.md`):** schema-drift detection/alerting and
> synchronous pre-upload schema rejection. Today validation is **lenient**: files are
> accepted + staged, then validated per-row at process time with quarantine — there is
> no up-front "reject the whole file if it doesn't match a declared schema."

## Key Packages Used

- `pkg/pipeline/` - format detection, validation, normalisation, enrichment logic
- `pkg/store/datalake/` - Parquet read/write, Delta Log management
- `pkg/store/objects/` - read source files from filesystem/S3
- `pkg/events/` - publish pipeline events to NATS (file ingested, quarantined)
- `pkg/ingest/` - the shared audience-ingest processor (ADR 0007): both the drop-zone
  worker and the gateway upload run files through `ingest.Processor.Process`

> **Drop-zone is INTERNAL-ONLY (external delivery deferred).** The poller +
> processor are real, but they presuppose a file already in the onboarding bucket.
> We don't support direct bucket access, so external providers can't self-deliver —
> files arrive via platform-managed S3 creds (staff/ops or an internal feed). The
> customer-facing ingestion path is the authed gateway upload
> (`POST /v1/api/audiences`), which also handles big files (202 → the same worker).
> To open it externally: presigned prefix-scoped PUT URLs, an authed streaming
> upload, or per-provider creds + per-prefix IAM (see ADR 0007).

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
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
