# pkg/attribution - Multi-Touch Credit Apportionment

Pure, model-independent attribution math: splits one conversion's credit across
its chain of ad exposures (multi-touch). No I/O, no storage, no billing — those
live in `cmd/reporting` and `pkg/store/analytics`.

## Key Entry Points

- `Apportion(chain []Touchpoint, conversionAt time.Time, model string) []Credit`
  (`apportion.go`) — the whole API. Returns fractional credit per touchpoint.
- Model constants: `ModelLastTouch` (billing default), `ModelFirstTouch`,
  `ModelLinear`, `ModelTimeDecay` (7-day half-life), `ModelPositionBased`
  (U-shape: 40% first / 40% last / 20% shared middle; n=2 splits 50/50).

## Invariants & Gotchas

- **Reporting-only.** Billing ALWAYS settles last-touch (see the chain write in
  `cmd/reporting/attribution.go` `recordChain` — "billing settles last-touch
  regardless"). Never wire `Apportion` output into a settle.
- **Apportioned on READ, stored model-agnostic.** The chain is one row per
  exposure in ClickHouse `attribution_touchpoints`
  (`pkg/store/analytics/clickhouse.go`, `AttributionWriter` /
  `AttributionChain`); `cmd/reporting/attribution_api.go` calls `Apportion` per
  request. Adding a model = one switch case here, zero reprocessing/migration.
- Fractions for a non-empty chain sum to 1; empty chain → `nil`. Input order is
  irrelevant (sorted oldest→newest internally). Unknown model falls back to
  last_touch — callers pass user-supplied `?model=` strings unvalidated.
- Chain writes are best-effort (`recordChain` WARNs and moves on); a chain-write
  failure must never block the last-touch settle.
- The rest of attribution is NOT here: exposure matching, click/view windows,
  confidence-floored identity resolve (`attribution.min_identity_confidence`,
  deterministic-only by default) = `cmd/reporting/attribution.go` + the
  `attribution.*` keys in `pkg/config/keys/reporting.go`; per-campaign overrides
  = `cmd/reporting/attribution_config_source.go` (`targeting_rules.attribution_config`).
  `attribution.model` (config) governs the write/billing side and only
  implements last_touch — the models above are read-time views.
- Window-boundary precision (e.g. ms-exact `since` binding so a zero-hour
  per-campaign window can't leak a same-second-prior exposure) lives in
  `ViewableImpressionsForUsers` in `pkg/store/analytics/clickhouse.go`, not here.
- Privacy Sandbox ARA is a separate reporting-only overlay in `pkg/ara`
  (never joined into exact conversions) — see `docs/attribution-phase4-ara.md`.

## Used By

- `cmd/reporting` — `attribution_api.go` serves
  `GET /v1/reporting/attribution?conversion_trace=…&model=…` (default: linear;
  tenant-checked — conversion attribution is advertiser data), proxied to the
  portal via `routes.APIAttribution` (`/v1/api/attribution`).

## Testing

Pure unit tests in `apportion_test.go` (sum-to-1, ordering, per-model shapes).
No fakes needed. End-to-end coverage rides the attribution e2e suite (see
`docs/attribution-plan.md` Phase 3).

## Docs

- `docs/attribution-plan.md` — phases 0–4 (Phase 3 = this package's multi-touch)
- `docs/attribution-gaps-plan.md` — hardening gaps G1–G7
- `docs/PLAN.md` -> "View-Through Conversion Attribution"
