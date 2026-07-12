# Data & reporting — dual-write + hot/cold read path

How an event becomes a number on a dashboard: **dual-written** to the hot store
(ClickHouse) and the cold store (Delta lake) from the same NATS stream, then read
back server-side through the metrics engine and the hot/cold router.

See also: [`e2e-trace`](e2e-trace.md) (the full request), [`data-pipeline`](data-pipeline.svg)
(the lake internals), [`architecture`](architecture.svg) (the map).

## Write path — one stream, two stores (independent NATS groups)

```mermaid
flowchart LR
  TR[Tracker / Exchange] -- publish --> N(("NATS<br/>events + auction.win"))

  subgraph HOT[" Hot store "]
    REPc[Reporting<br/>NATS consumer<br/>+ ClickHouse batch] --> CH[(ClickHouse)]
    ROLL[Rollup engine] --> RUP[(minute · hourly<br/>daily · monthly)]
    CH -. summing MVs .-> RUP
  end

  subgraph COLD[" Cold store "]
    PIPEc[Pipeline<br/>datalake sink<br/>ack-after-flush] --> LAKE[(Delta lake<br/>Parquet + Delta log)]
    COMP[Compaction<br/>CronJob :05] -.-> LAKE
  end

  N -- "consume (reporting group)" --> REPc
  N -- "consume (pipeline group)" --> PIPEc
  REPc --> ROLL
```

## Read path — server-side, tenant-scoped, hot/cold merged

```mermaid
flowchart LR
  P[Portal<br/>advertiser / publisher / staff] -->|"/v1/api/reports"| GW[Gateway<br/>enforceReportTenant<br/>injects account/publisher]
  GW -->|"/v1/reporting/query"| ENG[QueryEngine<br/>ecpm · ctr · fill_rate · net_revenue]
  ENG --> BLD[Builder · AutoTier<br/>rollup vs raw]
  BLD --> HC{HotColdStore<br/>route by age}
  HC -->|recent| CH[(ClickHouse<br/>HOT)]
  HC -->|aged · delta_scan| LAKE[(Delta lake<br/>COLD)]
  HC -->|spanning| MERGE[split + additive merge]
  CH --> MERGE
  LAKE --> MERGE
  MERGE --> ENG
  ENG -->|rows| P
```

## What to notice

- **Dual-write, not a mover** — reporting → ClickHouse (hot), pipeline → lake
  (cold), both from the *same* events via *independent* NATS consumer groups. No
  scheduled hot→cold copy; `hot_window` is a read-routing boundary only.
- **All business math is server-side** — the `QueryEngine` computes
  ecpm/ctr/fill_rate/net_revenue; the portal only renders (no browser math).
- **Tenant scope is injected at the gateway** (`enforceReportTenant`) and rides
  through *every* sub-query the engine issues — the isolation invariant.
- **Rollups are transparent** — `AutoTier` serves pre-aggregated rows when the
  range/metrics allow, else falls back to raw; correctness never depends on them.
- **Cold reads use `delta_scan`** over the real Delta log (the lake is a genuine
  Delta table); `cold_store_enabled` + the duckdb build tag gate it, off → hot-only.
