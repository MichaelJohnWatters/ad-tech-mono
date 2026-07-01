# ADR 0002 — Raw data pipeline: log-centric spine, Parquet archive, ClickHouse when it hurts

**Status:** Proposed (2026-07-01). One decision still open (see "Open question").
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

## Recommendation for where we are (small; values zero-slippage + trace visibility)

1. **Make JetStream + the Parquet pipeline the durable backbone.** Get *everything*
   reliably landing in Parquet on Minio. Highest-value work — it's the "on disk, forever"
   guarantee.
2. **Serve interactive/recent queries from a small store.** At current volume even the
   memory/DuckDB analytics backend covers Trace-Explorer point-lookups; no ClickHouse needed
   yet.
3. **DuckDB-over-Parquet for ad-hoc / historical / ML** — reads the lake directly, no extra
   infra.
4. **Add ClickHouse when an interactive dashboard is genuinely too slow at our volume** —
   a real signal, not a guess. Load it via a **minutely micro-batch** (one bulk INSERT/min):
   the ideal ClickHouse write pattern (avoids "too many parts") and gives ~1-min-fresh live
   dashboards (great for clicks).

Lean summary: **log + Parquet archive = the raw-data foundation; a small serving store for
recent point-lookups; ClickHouse is a later optimization for live aggregate dashboards.**

## Current state (what's built vs not — 2026-07-01)

| Piece | State |
|---|---|
| Event log (NATS JetStream) | ✅ built |
| Pipeline → minutely Parquet + Delta log (Minio) | ✅ built (`cmd/pipeline/datalake_sink.go`; flush interval configurable — set ~60s for minutely) |
| Analytics store interface (memory / DuckDB / ClickHouse) | ✅ built; **default = `memory` (volatile, in reporting RAM)** |
| ClickHouse backend + local pod | ✅ built + running (`:9010`), but **single-row inserts** (not batched) and **not the default** |
| Rollup engine (minute→monthly), idempotent, scheduled | ✅ built (`pkg/store/rollup`, `cmd/reporting/rollup.go`; off by default) |
| Trace Explorer (point lookup by trace_id) + batch reconciliation | ✅ built |
| **ClickHouse minutely micro-batch writes** | ⬜ not built (needed before ClickHouse handles heavy write load) |
| **Parquet compaction (minutely → hourly, aggregated)** | ⬜ not built |
| **DuckDB-over-Parquet query surface (`read_parquet`)** | ⬜ not built (ADR 0001's DuckDB role) |
| **Rollup read-by-tier** (query API reads `rollups` via `TierForRange`) | ⬜ stub (`pkg/reporting/builder.go`: `_ = tier`) |
| Rollup → Parquet target (land rollups in the lake, not just the DB) | ⬜ not built |
| Reporting as a normal pod (CGO-free now) / ClickHouse as local default | ⬜ not built (ADR 0001 roadmap) |

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

1. **Bulletproof the spine.** Pipeline reliably lands every event to Parquet on Minio at ~1
   min flush; verify replay-from-JetStream rebuilds it. (Most of this exists — harden +
   verify.)
2. **DuckDB-over-Parquet read surface.** A small query path (`SELECT ... FROM
   read_parquet('s3://…')`) so ops/dashboards can read the lake. Proves the read side E2E.
3. **Parquet compaction job.** Minutely → hourly aggregated Parquet (idempotent; dedup-safe),
   as a CronJob. Fixes small-files + gives cheap pre-aggregates.
4. **(When dashboards hurt) ClickHouse micro-batch loader.** Buffer events, flush one bulk
   INSERT/min; flip `reporting.analytics_backend=clickhouse`; wire rollup read-by-tier.
5. **(Cleanup) reporting → normal pod**, ClickHouse as local-overlay default (ADR 0001).

Until step 4, `memory` stays the default analytics backend so dev/CI/e2e are unaffected.
