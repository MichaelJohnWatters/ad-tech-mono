# pkg/pacing - Budget Pacing Calculations

Pure bid/no-bid throttling math: decides whether a campaign should bid on this
request or skip it to spread its daily budget across the day. Runs inside the
DSP's per-campaign bid loop on every bid request.

## Key Entry Points (`pacing.go`)

- `pacing.New(clk clock.Clock, cfg Config) *Pacer` — one cheap `Pacer` per
  campaign per request; clock-injected for testability.
- `Config{Mode, DailyBudget, DayStartUTC, Timezone}` — per-line-item settings.
- `Pacer.ShouldBid(actualSpend float64) bool` — the decision. Probabilistic
  throttle banded on the pacing ratio (behind pace → bid 100%, far ahead → 10%).
- `Pacer.TargetSpend() float64` — what should have been spent by now (mode curve
  × elapsed fraction of day).
- `Pacer.PacingRatio(actualSpend float64) float64` — actual/target; <1 behind, >1 ahead.
- Modes: `ModeEven` (linear), `ModeASAP` (bid until exhausted), `ModeFrontLoaded`
  (80% by noon). `cmd/dsp/main.go` `pacingMode()` maps campaign strings to these.

## Invariants & Gotchas

- **Pure and stateless — keep it that way.** No I/O, no Redis, no store calls.
  The caller supplies `actualSpend`; the DSP reads it from its in-process
  `BudgetTracker` mirror (hot-path iron rule: no per-call network I/O in the bid
  loop — see `cmd/dsp/refresh.go` / `cmd/dsp/CLAUDE.md`).
- **`ShouldBid` conflates over-budget with throttling.** It also returns false
  when spend exceeds budget, so the DSP checks exhaustion FIRST (before the
  pacer) to get its re-armed one-shot `BudgetDepletedEvent` publish — don't
  reorder that (comment at the callsite in `cmd/dsp/main.go`).
- **Non-deterministic by design** (except ASAP): throttling uses `rand.Float64()`
  so skips distribute randomly instead of bursting. Unit tests assert the
  deterministic pieces (`TargetSpend`, `PacingRatio`) with `clock.NewFake`.
- `TargetSpend() <= 0` (day not started / zero budget) → `ShouldBid` returns
  true; the caller's exhaustion check is what gates zero-budget campaigns.
- `Config.Timezone` is currently unused — the day boundary is `DayStartUTC` only
  (DSP passes `clk.Now().Truncate(24h)`).
- **Multi-replica correctness lives in the CALLER, not here.** The spend input
  is kept honest across replicas by the DSP's win-counter + reconcile from the
  `adtech.billing.campaign_spend_snapshot` broadcast (`cmd/dsp/pacing_reconcile.go`,
  gated by `dsp.spend_reconcile_enabled`) and reporting's shared additive Redis
  committed-spend counter (`reporting.shared_pacing_counter`, default ON — see
  `pkg/config/keys/reporting.go`, plus `reporting.pacing_reconcile_interval` /
  `reporting.pacing_counter_ttl`). Details in `cmd/dsp/CLAUDE.md` →
  "Budget accounting (two meters, reconciled)".
- PLAN's "Custom curve" mode and 60s "Pacing Feedback Loop" (PID controller,
  Redis-stored throttle_pct) are NOT implemented here — only the three modes +
  banded probabilistic throttle exist. The code's ratio bands match PLAN's table.

## Used By

- `cmd/dsp` (only consumer) — per-campaign in the bid evaluation loop.

## Pointers

- `docs/PLAN.md` → "17. Pacing Algorithms" (modes, ratio bands, throttling rationale)
- `docs/PLAN.md` → "Budget Handling", "Budget Management" (where spend truth comes from)
