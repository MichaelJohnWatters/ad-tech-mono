# E2E trace — one ad request across all planes

The platform's north star: *trace any ad request end-to-end with zero data
slippage.* This is that trace — **one `trace_id`**, created at the SSP and
threaded through every hop (HTTP `X-Trace-Id` / W3C `traceparent` / NATS message
header), across all five planes: **serving → async/events → data → money →
control**.

Renders on GitHub. For the structural map see [`architecture`](architecture.svg);
for the index + legend see [`README`](README.md).

```mermaid
sequenceDiagram
  autonumber
  actor B as Browser / SDK
  participant PA as Pub Ad Server
  participant SSP as SSP
  participant EX as Exchange
  participant DSP as DSP
  participant RDS as Redis
  participant AS as Ad Server
  participant TR as Tracker
  participant N as NATS
  participant IDC as Identity Consumer
  participant REP as Reporting
  participant CH as ClickHouse (hot)
  participant LAKE as Parquet export (cold)
  participant TB as TigerBeetle
  participant GW as Gateway / Portal

  Note over B,GW: one request = one trace_id, threaded through every hop

  rect rgb(238,242,255)
  Note over B,RDS: 1 · Request & auction (sync RTB)
  B->>PA: page ad slot
  PA->>SSP: ad request — trace_id created here
  SSP->>RDS: audience segments — user + household (25ms budget)
  SSP-)N: adtech.identity.observed
  SSP->>EX: OpenRTB bid request — user.ext.segments + household EID stamped
  EX->>DSP: fan-out (OpenRTB)
  DSP->>RDS: budget + prepay-balance gate
  DSP->>RDS: private segments (dsp_private) unioned before targeting
  DSP-->>EX: bid
  EX->>EX: first-price auction + deal priority
  EX-)N: adtech.auction.win — single source of truth for cost
  DSP->>RDS: record local win-notice spend (fast, approximate)
  end

  rect rgb(236,253,245)
  Note over EX,TR: 2 · Serve & track (sync)
  EX->>AS: serve winning creative
  AS-->>EX: markup + pixel URLs
  EX-->>SSP: BidResponse (winner)
  SSP-->>PA: ad markup
  PA-->>B: render ad
  B->>TR: impression pixel (same trace_id)
  end

  rect rgb(250,245,255)
  Note over N,CH: 3 · Async fan-out — single write (data), independent NATS groups
  TR-)N: adtech.events.impression
  N-)IDC: consume identity.observed (own group)
  IDC->>IDC: batch + dedup → upsert identity_graph edges (PG)
  N-)REP: consume
  REP->>CH: write impression — HOT store (the single analytical store)
  end

  rect rgb(255,251,235)
  Note over REP,DSP: 4 · Money loop
  REP->>TB: accrue / settle spend (double-entry)
  REP->>RDS: advertiser prepay-balance drawdown
  REP-)N: adtech.billing.campaign_spend_snapshot
  N-)DSP: reconcile budget counter to billed truth (corrects the fast path)
  end

  rect rgb(241,245,249)
  Note over GW,LAKE: 5 · Report (read path, later)
  GW->>REP: /v1/api/reports — tenant scope injected
  REP->>CH: recent range (HOT)
  REP->>LAKE: aged range via ClickHouse s3() (COLD)
  REP->>REP: HotColdStore merges + QueryEngine derived metrics
  REP-->>GW: rows → portal renders (no browser math)
  end
```

## What to notice (the invariants)

- **`trace_id` is the spine** — one value from the SSP through serving, NATS,
  logs, the analytics store, and the billing ledger. It's the OTel W3C trace ID;
  Grafana pivots Loki↔Jaeger on it.
- **`adtech.auction.win` is the single source of truth for cost** — billing and
  reporting both derive from it, not from independent guesses.
- **Single write, derived cold archive (§3)** — reporting writes ClickHouse (hot)
  from the NATS stream; the Parquet cold archive is exported *from* ClickHouse
  hourly (the `ch-parquet-export` chain step), not written by a second consumer.
- **Money is fast-then-correct (§1, §4)** — the DSP paces on a local win-notice
  in Redis (instant, approximate), and later reconciles to the billing ledger's
  committed-spend snapshot (authoritative). See `billing-flow`.
- **The cold archive can't drift from hot (§3)** — it's a derived hourly export
  of the ClickHouse tables, re-exported per changed hour, so it always matches
  what the hot store holds.
- **Reads route by age (§5)** — recent → ClickHouse, aged → the Parquet export via
  ClickHouse `s3()`, spanning ranges merged; tenant scope is injected at the
  gateway and rides through every sub-query.
- **Targeting data is read-only on the hot path (§1)** — segment lookups hit
  Redis inside a 25ms budget and degrade to "no segments", never blocking the
  bid; identity-graph *writes* happen off-path via the identity consumer (§3).
  How that data gets built and loaded: see `targeting-data-flow`.
