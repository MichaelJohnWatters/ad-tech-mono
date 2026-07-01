# ADR 0003 — Optimiser telemetry: routing/bandit outcomes → ClickHouse, warm-start from it

**Status:** Proposed (2026-07-01).
**Related:** ADR 0002 (raw-data pipeline — batched ClickHouse ingest, rollups, Parquet); ADR 0001 (analytics engines).

## Context

An audit of "what's still in-memory that should be columnar" found the event/analytics
layer is now genuinely on ClickHouse (ADR 0002), with one real gap and one adjacent
volatility issue:

1. **Smart-router / bandit outcome telemetry is in-memory only.** The exchange's
   `optimise.SmartRouter` (`pkg/optimise/routing.go`) records per-DSP call outcomes via
   `RecordCall(channel, dspID, bidReceived, bidPrice, latency, timedOut)` and `RecordWin`,
   holding them in a `map[channelDSPKey]*DSPStats`. These are **never emitted as events**.
   Consequences:
   - No historical analysis — you can't ask ClickHouse/DuckDB "why did routing skip DSP X
     between 2–3pm?" The per-decision history dies with the pod.
   - **Cold after restart** — a rescheduled exchange pod re-learns routing from zero.
   - `pkg/optimise/bidopt.go` is documented to *"consume historical data from the analytics
     store"* — the optimiser is designed to read analytics the routing path doesn't write.

   The router's *live* state should stay in memory (it's read on the hot auction path — no
   ClickHouse round-trip per bid). What's missing is emitting the *outcome events* to the
   log so they land in ClickHouse (hot) + Parquet (cold), and warm-starting the in-memory
   router from that history on boot.

2. **Billing ledger defaults to `memory` (volatile).** TigerBeetle is wired
   (`billing.ledger_backend=tigerbeetle`) but not the default; the local Tiltfile doesn't set
   it, so local billing spend is lost on restart. This is a *ledger* (transactional), so the
   durable answer is TigerBeetle, **not** ClickHouse — tracked here only because it's the
   other "still in-memory" item. Low effort, separable.

Prometheus collectors, `MemoryL2` (Redis fallback), `datalake.MemoryStore` (tests), and the
DSP `depletedAlreadyPublished` sync.Map are correctly ephemeral — out of scope.

## Decision

Emit per-DSP-call outcome events onto the NATS log (same fan-out as every other event → hot
ClickHouse via reporting, cold Parquet via pipeline). Keep the in-memory `SmartRouter` as the
hot-path decision cache, but **warm-start it from ClickHouse** on boot so routing survives
restarts and has real history. Reuse the ADR 0002 machinery (batched ingest, rollups,
datalake archive) rather than inventing a new path.

## Plan

### Phase A — DSP call-outcome events → ClickHouse

1. **Event + subject** (`pkg/events/`)
   - `DSPCallEvent{ SchemaVersion, TraceID, AuctionID, Channel, DSPEndpoint, BidReceived,
     BidPriceUSD, LatencyMs, TimedOut, Won, PublisherID, Timestamp }` in `payloads.go`.
   - New subject `adtech.optimise.dsp_call` in `subjects.go` (+ add to the stream subject set
     already covered by `adtech.>`). Document it in `docs/PLAN.md` → "NATS Subjects".

2. **Exchange emits** (`cmd/exchange/main.go`, `auctionHandler`)
   - At the existing `router.RecordCall` / `router.RecordWin` sites, also publish one
     `DSPCallEvent` per DSP per auction via the existing `*events.Publisher` (fire-and-forget;
     never block or fail the auction on publish error — same posture as auction events).
   - Keep `RecordCall`/`RecordWin` exactly as-is (live routing state unchanged).
   - Gate with `exchange.emit_dsp_call_events` (default true) + an optional sample ratio
     `exchange.dsp_call_sample_ratio` (default 1.0; tracker-style sampling if volume bites,
     since it's one event per DSP per auction).

3. **ClickHouse table + ingest** (`pkg/store/analytics/`, `cmd/reporting/`)
   - `dsp_calls` MergeTree table (ORDER BY timestamp) in `clickhouse.go` `createTables`;
     mirror rows in `memory.go`/`duckdb.go`.
   - Add `InsertDSPCalls(ctx, []*DSPCallEvent) error` to the `BatchInserter` interface and all
     three backends (ClickHouse via `PrepareBatch`; memory/duckdb loop).
   - Reporting batch handler `handleDSPCallBatch` (reuses `batchProcess` from
     `cmd/reporting/batch.go`, no billing side-effect) on `adtech.optimise.dsp_call`; add the
     subject to `coreBatchHandlers()`. Per-message fallback handler for the non-batch path.

4. **Rollup** (`pkg/store/rollup/rollup.go`)
   - `DSPCallsConfig{ Source:"dsp_calls", Dimensions:[channel, dsp_endpoint],
     Metrics:[count, sum_bid, sum_latency_ms, sum_won, sum_bid_received] }` (all additive so
     tiered read-by-tier works; derive bid_rate/win_rate/avg_latency at read time = ratios of
     sums). Register in `cmd/reporting/rollup.go`. Add to `builder.tableToRollupConfig` +
     `rollupDimensions`.

5. **Cold archive** (`cmd/pipeline/datalake_sink.go`)
   - Add `adtech.optimise.dsp_call` → `dsp_calls` to `eventTables` with a matching schema so
     the pipeline lands it in Parquet (DuckDB-queryable like every other table).

### Phase B — Warm-start SmartRouter from ClickHouse

6. **Boot-time seed** (`cmd/exchange/main.go`, `pkg/optimise/routing.go`)
   - Add `SmartRouter.Seed(stats []DSPStats)` (or `SeedFromRollup`) that pre-populates the
     in-memory map from an aggregate.
   - On exchange boot, query the last N hours of `dsp_calls` rollups — via the reporting query
     API (`POST /v1/reporting/query`, table `dsp_calls`, group by channel+dsp_endpoint) or a
     direct ClickHouse read — and `Seed` the router. Config `exchange.routing_warmstart`
     (default true); on any error log + start cold (never block boot).
   - This closes "cold after restart" without moving hot-path state out of memory.

### Phase C — Bandit: read existing events, no new pipeline (lower priority)

7. The creative bandit (`pkg/optimise/bandit.go`) can recompute arm stats from the
   **impressions + clicks already in ClickHouse** — no new event stream needed. Add a
   warm-start query (impressions/clicks grouped by creative over a recent window) to seed arm
   weights on boot, mirroring Phase B. Defer unless creative optimisation is being worked on.

### Phase D — Local billing durability (separable, optional)

8. Flip the local overlay to `billing.ledger_backend=tigerbeetle` (TB pod is up; native
   memory pre-allocation already tuned per `project_tigerbeetle_ledger`). No code change —
   env var in the Tiltfile reporting resource. Verify spend survives a reporting restart.
   Independent of A–C; do it whenever durable local spend is wanted.

## Files

| File | Change |
|---|---|
| `pkg/events/payloads.go`, `subjects.go` | `DSPCallEvent` + `adtech.optimise.dsp_call` |
| `cmd/exchange/main.go` | emit event at RecordCall/RecordWin (gated, fire-and-forget) |
| `cmd/exchange/config.go` | `exchange.emit_dsp_call_events`, `dsp_call_sample_ratio`, `routing_warmstart` |
| `pkg/optimise/routing.go` | `Seed(...)` warm-start entry point |
| `pkg/store/analytics/{analytics,clickhouse,clickhouse_batch,memory,duckdb_batch}.go` | `dsp_calls` table + `InsertDSPCalls` on `BatchInserter` |
| `cmd/reporting/batch.go`, `main.go` | `handleDSPCallBatch` + register subject |
| `pkg/store/rollup/rollup.go`, `cmd/reporting/rollup.go` | `DSPCallsConfig` + register |
| `pkg/reporting/builder.go` | `dsp_calls`→config mapping + dimensions |
| `cmd/pipeline/datalake_sink.go` | `dsp_calls` in `eventTables` |
| `Tiltfile` | (Phase D) `BILLING_LEDGER_BACKEND=tigerbeetle` |

## Reuse (don't reinvent)
- ADR 0002 batched ingest (`BatchInserter`, `SubscribeBatch`, `batchProcess`) — the event
  rides the exact same path as impressions.
- Rollup framework + `TierForRange` read-by-tier — bid_rate/win_rate come free as ratios of
  additive sums.
- `events.Publisher` (already in `auctionHandler`) — no new NATS wiring.
- Existing `SmartRouter.Stats()` shape for the seed payload.

## Verification
1. Run auctions; `SELECT count(), sum(won) FROM dsp_calls GROUP BY dsp_endpoint` in ClickHouse
   reconciles with `/debug/exchange/routing` (in-memory) and the Prometheus `auctionsWonTotal`.
2. Restart the exchange pod → routing decisions match pre-restart within the warm-start window
   (not cold) — assert `SelectDSPs` preview is stable across restart.
3. DuckDB `delta_scan('s3://…/dsp_calls')` count reconciles with the ClickHouse count (hot/cold
   agree).
4. Rollup read-by-tier: a 24h routing report returns from the hourly `dsp_calls` rollup.
5. (Phase D) restart reporting → billing ledger balances survive (TigerBeetle).
6. `make test` green; new unit tests: exchange emits one event per DSP per auction (fake
   publisher); `handleDSPCallBatch` inserts N→1; `SmartRouter.Seed` reproduces stats.

## Effort / risk
- **A**: medium — new event type threaded through the ADR 0002 machinery (mechanical, follows
  the impression pattern). Main risk: volume (one event per DSP per auction) — mitigated by the
  sample-ratio knob and the batched consumer.
- **B**: small — one query + one seed method, fully fail-open.
- **C**: small, deferrable — pure read of existing data.
- **D**: trivial — one env var; independent.

Recommended order: A → B (they pay off together: emit, then consume), D any time, C when
creative optimisation is next touched.
