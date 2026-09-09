# cmd/optimise - Placeholder (no binary)

**This directory is intentionally empty** (`.gitkeep` only). There is no main.go,
no Dockerfile in `build/`, no Helm CronJob, and no Makefile target. PLAN step 63
was CLOSED 2026-08-09 with the verdict "no standalone binary needed" — both
optimisation seams run **in-process** (see `docs/PLAN.md` -> "Build Status &
Outstanding Work", ledger row 63).

## Where the optimisation engine actually lives

Core library: `pkg/optimise/` (pure, unit-tested). In-process consumers:

- **Creative rotation (multi-arm bandit)** — `cmd/adserver`: `optimise.NewBandit`
  (Thompson sampling, `pkg/optimise/bandit.go`) selects creatives at serve time;
  `cmd/adserver/warmstart.go warmStartBandit` boot-seeds arms from per-creative
  CTR so a fresh pod doesn't restart exploration cold.
- **DSP routing (SmartRouter)** — `cmd/exchange`: skip/rank fan-out targets from
  live bid/timeout stats (`pkg/optimise/routing.go`); `warmstart.go` boot-seeds
  from global `dsp_calls` (fail-open, never blocks boot), `routingsync.go` does
  the periodic cross-replica reseed + idempotent reset broadcast. All thresholds
  are `exchange.routing_*` live config keys (`pkg/config/keys/exchange.go`)
  wired via `router.SetKnobs` (kill-switch `routing_enabled`, `routing_min_calls`,
  ε-probe `routing_explore_pct`, `routing_never_skip` deal allowlist).
- **Dormant, no callers yet** — `BidOptimiser`/`ScorePlacements` (bidopt.go),
  `RebalanceBudgets` (budget.go), `GenerateRecommendations` (recommendations.go)
  await a runner (this CronJob, if it's ever built).

## Gotchas

- Older PLAN.md prose (and the `cmd/CLAUDE.md` jobs table) still say things like
  "updated hourly/daily by `cmd/optimise/`" (bid shading curves, quality scores,
  recommendations, floor pricing). The ledger row is the truth: those are either
  in-process or not yet wired.
- If you DO build a binary here, reconcile the PLAN ledger first and follow the
  job pattern in `cmd/CLAUDE.md` (`batch-conductor` is the exemplar for
  scheduled analysis).
