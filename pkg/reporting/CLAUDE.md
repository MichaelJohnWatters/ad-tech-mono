# pkg/reporting - Report Builder + Metrics Engine

Query construction and server-side business metrics for the analytics store.
Pure library — no HTTP, no NATS, no DB driver. `cmd/reporting` wraps it behind
the query API; portals only ever render, never do business math.

## Key Entry Points

- `QueryEngine` (`engine.go`) — `NewQueryEngine(store, net).Query(ctx, params)`.
  Drop-in for `analytics.Store.Query`: a query with no derived metrics passes
  straight through unchanged (raw path stays byte-identical). Derived metrics
  are composed server-side from base sub-queries.
- Derived-metric registry (`metrics.go`) — `ecpm`, `ctr`, `fill_rate`,
  `viewability_rate`, `completion_rate`, `net_revenue`. Ratios emit as
  percentages; compute failure emits null (client renders "—").
- `NetResolver` (`metrics.go`) — interface for net_revenue so this package
  never imports `pkg/billing`; `cmd/reporting` adapts its warm
  `billing.ContractStore` (`contractNet` in `cmd/reporting/main.go`).
- `Builder` (`builder.go`) — fluent query builder over `analytics.Store`
  (`Table/Metrics/GroupBy/Filter/ForAccount/TimeRange/Build`). `AutoTier()`
  serves eligible queries from pre-aggregated rollups via
  `rollup.TierForRange` + `analytics.RollupReader`, else raw fallback.
- Pre-built `Report` templates (`builder.go`) — `AllReports()`:
  campaign/creative performance, geo breakdown, publisher yield.
- `HLL` + `FrequencyDistribution` + `ForecastReach` (`reach.go`) — HyperLogLog
  reach sketches, frequency buckets, reach forecasting.

## Invariants & Gotchas

- **Tenant scope rides on `params.Filters` — the caller supplies it.** This
  package does NOT read `auth.AccountIDFromContext`; `cmd/reporting`/gateway
  inject the account filter. The engine's guarantee: every derived-metric
  sub-query carries the SAME filters/dims/time as the request, so the tenant
  scope propagates through every base query (no cross-tenant leak). Order and
  limit are applied only AFTER composition.
- **`net_revenue` requires publisher scope** — group by `publisher_id` or
  filter to a single one; the engine hard-errors otherwise (never blends the
  fallback fee across mixed publishers).
- **Rollup correctness rule: fallback is guaranteed.** `AutoTier` only serves
  from rollups when the table maps (`impressions`→events, `auctions`→auctions),
  every non-time group-by/filter is a dimension the rollup carries, and every
  metric is additive (`count`, `sum_*`). Anything else — or zero matching
  rollup rows — falls back to a raw scan. Correctness never depends on rollups.
- **`rollupDimensions` must track `rollup.Config`** (pkg/store/rollup). Drift
  is safe-but-slow: unknown dims force raw scans, not wrong answers.
- **`HLL.Merge` silently no-ops on precision mismatch** — construct both sides
  with the same precision. Merge is otherwise lossless (union).
- `reach.go` has no in-tree production callers yet (tests only) — planning
  surface, don't assume it's wired.
- Composite keys join dim values with `\x00`; base values key as
  `table\x00metric` in `computeCtx`.

## Used By

- `cmd/reporting` only — the query API (`routes.ReportingQuery`) and staff
  channel breakdown (`staff_channels.go`) wrap `QueryEngine`.

## Testing

Pure logic, plain `go test` (no tags, no containers): `builder_test.go`,
`builder_tier_test.go`, `engine_test.go`, `reach_test.go`.

## PLAN.md Pointers

- `docs/PLAN.md` -> "Custom Report Builder"
- `docs/PLAN.md` -> "Data Rollups", "Universal Rollup Framework"
- `docs/PLAN.md` -> "Reach and Frequency Reporting", "Reach/Frequency Forecasting and Campaign Planning"
- `docs/PLAN.md` -> "Variable Margin and Revenue Share" (net_revenue contracts)
