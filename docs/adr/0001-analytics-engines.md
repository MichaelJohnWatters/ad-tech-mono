# ADR 0001 — Analytics engines: ClickHouse (event store) + DuckDB (ad-hoc over the data lake)

**Status:** Accepted (2026-07-01)
**Supersedes:** the earlier "DuckDB local / ClickHouse prod, swap by environment" framing in `docs/MOCK_AUDIT.md` (A1).

## Context

The analytics layer sits behind the `analytics.Store` interface (`pkg/store/analytics`).
Three concrete backends have been discussed:

- **memory** — volatile, in-process. Default; used by unit tests, CI, and the e2e harness's debug read-backs.
- **DuckDB** — embedded, single-file, columnar. Real, selectable (`reporting.analytics_backend=duckdb`). Driver is **CGO**.
- **ClickHouse** — server/cluster, columnar, built for high-ingest + real-time aggregation. Not yet implemented. Go client (`clickhouse-go/v2`) is **pure Go**.

Separately, `pkg/store/datalake` writes real **Parquet + a Delta-style log** to object storage (A3).

The original plan treated DuckDB and ClickHouse as environment-swapped alternatives for the *same* role (the event store). That framing has two problems:

1. **CGO tax.** DuckDB's CGO driver is why the reporting service can't build like every other service (`CGO_ENABLED=0`) and why the Tiltfile runs it as a *local process* instead of a k8s pod. Keeping DuckDB on the hot serving path keeps that wart.
2. **Parity theatre.** "DuckDB local, ClickHouse prod" means the engine you test against is not the engine you ship. Divergence bugs hide there.

## Decision

Give the two engines **different jobs** instead of making them environment-swapped equivalents:

- **ClickHouse is the event store (`analytics.Store`) in every non-test environment** — local dev, staging, prod. It owns the real-time hot path: event ingest, dashboard aggregations, rollups.
- **DuckDB is repositioned** off the hot path into an **ad-hoc / cold query engine over the Parquet data lake** — backfills, historical/ad-hoc analysis, pipeline transforms. It reads the Parquet files in place (no ingest step), which is exactly what DuckDB is best at.
- **memory stays** as the zero-infra backend for unit tests / CI / the e2e debug read-backs.

So the `analytics.Store` selector becomes **`memory` (tests) | `clickhouse` (dev+staging+prod)**. DuckDB **drops out of the `analytics.Store` selector** and lives behind a separate, narrower surface for data-lake queries.

## Consequences

**Positive**
- Reporting returns to **`CGO_ENABLED=0`**, builds like every other service, runs as a normal k8s pod. The Tiltfile "reporting stays local for CGO" special-case goes away.
- **True dev/prod parity** — same engine everywhere the event store runs.
- Real-time serving handled by the engine actually designed for it.
- DuckDB's CGO requirement is isolated to a non-critical ad-hoc tool.
- A1's work is **not wasted**: the selector, operational-signal persistence, and rollup-writer plumbing are interface-level and carry straight over. The DuckDB `analytics.Store` backend remains a working option for anyone who wants an embedded local run; it's just no longer the recommended durable path.

**Negative / cost**
- ClickHouse locally = one more **server pod** (heavier than embedded DuckDB). Mitigated: the stack already runs Postgres/Redis/NATS/Minio/Jaeger locally, and a realistic local HA setup is a project goal. `memory` remains for anything that must boot with zero infra.
- New dependency (`clickhouse-go/v2`) + a ClickHouse StatefulSet in `k8s/`.

## Roadmap

1. Implement `pkg/store/analytics/clickhouse.go` against the `analytics.Store` interface (core events + operational signals + rollups for full parity with memory/DuckDB). Integration-test-tagged (needs a live CH), like the TigerBeetle ledger.
2. Add `clickhouse` to `selectAnalyticsStore`; keep `memory` default for tests, make `clickhouse` the dev/staging/prod value.
3. ClickHouse StatefulSet + `k8s/` wiring; Tiltfile runs reporting as a normal pod (drop the CGO special-case).
4. Reposition DuckDB: a small data-lake query surface (`SELECT ... FROM read_parquet(...)` over the A3 Parquet), used by the pipeline / an ad-hoc endpoint — not the `analytics.Store` hot path.

Until step 1 lands, `memory` remains the default and DuckDB remains the selectable durable option, so nothing regresses.
