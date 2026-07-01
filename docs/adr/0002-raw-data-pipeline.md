# ADR 0002 — Raw data pipeline: log-centric spine, Parquet archive, ClickHouse when it hurts

**Status:** Accepted (2026-07-01). ClickHouse is the raw-ingestion + hot-serving tier,
built into the local stack now (not deferred) — it's the de-facto AdTech event store
(Vibe, Admixer, AdGreetz: millions of events/sec, <100ms live reports). The earlier
"defer ClickHouse until dashboards hurt" stance was too conservative and contradicted the
full-local-build goal; corrected here.
**Related:** ADR 0001 (analytics engines: ClickHouse vs DuckDB roles); `docs/PLAN.md` → "Build Status & Outstanding Work".

## The problem we're solving

"What's the best way to handle *all our raw data*?" — the high-volume event firehose
(impressions, clicks, conversions, views, auction events, auction wins, media events).
NOT the transactional data (accounts, campaigns, budgets, deals) — that's settled in
Postgres, and money is settled in the billing ledger (memory today, TigerBeetle option).

## The reframe (the key idea)

**Don't pick one database for all raw data. There is a single source of truth — the
event log — and every query store is a disposable, rebuildable *view* on top of it.**

- If a downstream store is slow, wrong, or lost → **replay the log** and rebuild it.
- So the real questions are: (1) is the durable record safe? (2) what views do we need?

This is the log-centric / "kappa-lite" model. We already have the log: **NATS JetStream**.

## The architecture

### The durable spine (this is "handling the raw data")

```
events ──▶ NATS JetStream ──▶ pipeline ──▶ Parquet + Delta log on Minio (S3)
           (recent, replayable)            (permanent, open, cheap, replayable)
```

- **JetStream** = short/medium source of truth — a replay window bounded by disk retention
  (hours/days). Fast appends, ordered, durable.
- **Parquet lake (Minio)** = the forever archive. Open format; DuckDB / Spark / anything
  reads it. Once an event is in Parquet it's safe even after it ages out of JetStream.

Nail this spine first — it's what makes "zero data slippage" true **on disk**, not just in
a service's RAM. Everything below is optional and rebuildable from it.

### Views on top (chosen by read pattern)

Raw data is read two very different ways, which want different things:

| Read pattern | Example | Wants | Fit |
|---|---|---|---|
| **Aggregate over time** | impressions/spend by campaign, last 24h | columnar scan + rollups | ClickHouse **or** DuckDB-over-Parquet |
| **Point lookup by key** | Trace Explorer: show me *this* trace_id | index / sorted key | ClickHouse (ORDER BY); Parquet-scan works but degrades |

### Rollups

Minute → hourly (→ daily/monthly later). Two placement options:
- **In the serving store** (ClickHouse `SummingMergeTree` materialized views = native, or
  our app-side `rollup.Engine`, backend-portable).
- **In the lake** (compact minutely Parquet → hourly aggregated Parquet — fixes the
  "small files problem" and gives cheap pre-aggregates DuckDB can read).

Each rollup level re-aggregates from **raw** (not from a coarser rollup), so we can run just
minute (and hourly) now and add coarser tiers with zero code change.

## Recommendation (build the real prod shape locally)

1. **JetStream = the durable source-of-truth log.** Fast appends, ordered, replayable.
   Every downstream store is a rebuildable view on top of it.
2. **ClickHouse = raw-ingestion + hot-serving tier, built now.** Ingest the event stream via
   a **batched async writer** (buffer → one bulk INSERT/sec) — the single most important
   detail; single-row inserts are the "too many parts" anti-pattern. Raw events land in
   MergeTree; native `SummingMergeTree`/`AggregatingMergeTree` materialized views produce
   rollups; live dashboards read them in <100ms. Use CH's AdTech strengths: `uniqExact`/HLL
   (unique users), `windowFunnel` (funnel/attribution), `LowCardinality`+`ZSTD`/`Delta`
   codecs (compression).
3. **Parquet+Delta on Minio = the cold archive.** Async, permanent, open format. Survives
   after events age out of JetStream / ClickHouse TTL.
4. **DuckDB = ad-hoc / historical / ML over the Parquet archive.** No server; reads the lake
   directly (`read_parquet`). The analyst/ops/back-testing engine, complementary to
   ClickHouse's live serving — see ADR 0001.

Lean summary: **log (JetStream) is the source of truth; ClickHouse is the hot raw-ingest +
live-dashboard store (batched); Parquet is the cold archive; DuckDB queries the archive for
ad-hoc/historical work.**

## Current state (what's built vs not — updated 2026-07-01, phases 1–5 landed)

| Piece | State |
|---|---|
| Event log (NATS JetStream) | ✅ built |
| Pipeline → Parquet + Delta log (Minio) | ✅ built (`cmd/pipeline/datalake_sink.go`) |
| Analytics store interface (memory / DuckDB / ClickHouse) | ✅ built; code default `memory` (CI/e2e); **local stack default = `clickhouse`** |
| ClickHouse backend + local pod | ✅ built + running (`:9010`), **local default** |
| **ClickHouse batched ingest** | ✅ built — consumer-level batch (`BatchInserter` + `SubscribeBatch`), one atomic block per fetch + Redis dedup (phase 1) |
| **Rollup read-by-tier** | ✅ built — `reporting.Builder` reads rollups via `TierForRange`, raw fallback (phase 2) |
| **ClickHouse-native rollups** | ✅ built — `SummingMergeTree` MVs (impressions hourly/daily), `QueryRollups` routes to them (phase 3) |
| Rollup engine (minute→monthly), idempotent, scheduled | ✅ built; on in local overlay (owns minute/monthly/auctions; MVs own events hourly/daily) |
| **Parquet cold-archive verification** | ✅ built — `/debug/datalake/snapshot` reconciles TotalRows to the event count (phase 4) |
| **Parquet compaction** | ✅ built — `ObjectStore.Compact` bin-packs + Delta removes; `cmd/compact` CronJob (phase 5) |
| **DuckDB-over-Parquet query surface** | ✅ built — `datalake.ParquetReader` (httpfs + `delta_scan`/`read_parquet`), build tag `duckdb` (phase 5) |
| Trace Explorer (point lookup) + batch reconciliation | ✅ built |
| Rollup → Parquet target (land rollups in the lake, not just the DB) | ⬜ not built (optional; MV + compaction cover the hot/cold aggregate needs) |
| Reporting as a normal pod (CGO-free now) / ClickHouse as overlay default across envs | ⬜ not built (ADR 0001 roadmap; local runs clickhouse today) |

## Open question (resolve this to finalize)

**What's the dominant thing we do with *recent* raw data — scan-and-aggregate (dashboards)
or look-up-one-thing (trace/debug)?**

- Mostly **trace/debug one request** → keep recent events in a fast point-lookup store
  (ClickHouse-lite, or even Postgres for last N hours) + archive everything to Parquet; skip
  heavy OLAP for now.
- Mostly **charts/rollups** → columnar is king; DuckDB-over-Parquet now, ClickHouse (minutely
  batch) when it hurts.

Gut read from the project's priorities (Trace Explorer, zero slippage, small): **durable
spine + point-lookup on recent first, dashboards second.** So the next highest-leverage build
is making the **Parquet archive bulletproof and DuckDB-queryable**, not more ClickHouse.

## Proposed build order (for a fresh context to pick up)

1. **ClickHouse batched async writer.** Buffer events, flush on size **or** ~1s interval as
   one bulk INSERT; fail-open to the log so a CH blip never drops events. Test proves
   batching + idempotency. ← the one missing brick between "CH pod exists" and "CH is our
   event store".
2. **Flip local default to `clickhouse`** + wire rollup read-by-tier (replace the
   `pkg/reporting/builder.go` `_ = tier` stub) so dashboards read rollups, not raw. Keep
   `memory` as the CI/e2e default so those stay infra-free.
3. **CH-native rollups.** `SummingMergeTree`/`AggregatingMergeTree` materialized views;
   app-side `rollup.Engine` stays as the portable fallback.
4. **Cold archive: bulletproof the Parquet spine.** Pipeline reliably lands every event to
   Parquet on Minio; verify replay-from-JetStream rebuilds it. (Most exists — harden.)
5. **DuckDB-over-Parquet read surface** (`read_parquet('s3://…')`) for ad-hoc / historical /
   ML + **Parquet compaction** (minutely → hourly aggregated, idempotent CronJob).
6. **(Cleanup) reporting → normal pod**; ClickHouse as local-overlay default (ADR 0001).
