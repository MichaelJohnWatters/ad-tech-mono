# pkg/simulator - Programmable Traffic Simulator (library)

Go library for generating realistic ad traffic. The CLI (`cmd/simulator`), the demosite, the gateway's sim endpoints, and the e2e harness all build their traffic from here — one source of truth for what a simulated request/page/persona looks like.

## Key Entry Points

- `simulator.go` — `New(Config)` → `Simulator`. `RunSingle` POSTs an OpenRTB request to the exchange `/v1/openrtb/auction`, then fires the tracker `/v1/t/imp` pixel on a win. `RunProfile(profile, duration)` runs a named RPS profile: `trickle`=1, `steady`=10, `burst`=100, `stress`=500 (default 5). Uses `tracing.NewClientTraceparent()` so the exchange adopts the simulator's trace ID — one ID across Jaeger/Loki/NATS/analytics.
- `request/` — production-like OpenRTB. `Build(Input)` populates every field a service reads: schain, consent/regs, identity (UID2/hashed email/first-party), segments, format-specific imp (display/video/audio/native), ad pods (`Input.Pod`). `Persona` (persona.go) = who/where/consent; registries `Personas` (default mix) and `ThemedPersonas` (readable-world); `Pick` samples by weight. `QueryParams`/`RealismParams` (querybuild.go) render the same signals as query params for browser-driven paths (SSP `/v1/ssp/serve`, `/v1/pubad/*`) that build OpenRTB server-side.
- `pages/` — the SINGLE SOURCE OF TRUTH for external demo publisher pages. `Layout` (ordered ad `Slot`s, `CountByFormat()` = the e2e's assertion target) and `Site` (three publisher tenants with own placements via `PlacementByFormat()`). `cmd/demosite` renders these; `tests/e2e` replays them (`VisitPage`) and asserts the same N impressions land in ClickHouse — both import THIS package, so page and assertion can't drift.

## Invariants & Gotchas

- **`RealismParams` is THE single source for the TCF/GPP/US-privacy/UID2 encoding.** The web publisher-simulator fetches it via `GET /v1/sim/realism` (`cmd/gateway/sim.go`) instead of re-implementing it in JS; `cmd/gateway/sim_test.go` proves CLI and web paths agree. Change the encoding here only.
- **Pixel/beacon requests need a browser-shaped UA + Referer** — the tracker's fraud check drops the default Go UA as a bot. `RunSingle` sets both; any new caller must too (mirrors `tests/e2e/harness/tracker.go`).
- **`HouseholdPool` is load-bearing.** Without it, one persona = one fixed household forever: at load rates the serve layer freq-capped ~half of all cleared wins (2026-07-19), and the fixed-IP co-viewing pair once absorbed ~8% of ALL traffic (the 2026-08-05 "wall of adserver 429s"). `HouseholdShared` time-slots the pool (10-min) so co-viewing personas still land in the SAME household while it rotates. Don't zero these fields.
- **`ThemedPersonas` are deliberately NOT in the default registry** — perf baselines and load profiles depend on the exact default mix. Themed personas (Interests-biased browsing that coherently EARNS segment membership, incl. the GPC negative case) are reached via the "themed" profile or `--persona`. See `docs/AUDIENCE-PIPELINE.md`.
- **`UserPool`** gives a persona stable recurring user ids — required for frequency-rule (`min_count>1`) segment membership; the default per-request random id makes earning it impossible.
- GDPR-consented is presence-of-TCF-string only (the privacy engine checks presence, not decoded semantics); `residentialIP` deliberately avoids datacenter ranges so fraud scoring doesn't flag sim traffic.
- `pages.Format` string values are the `/v1/pubad/*` channel AND the analytics `channel` column — they flow unchanged slot → serve → beacon → ClickHouse. Renaming one breaks the zero-slippage e2e.

## Used By

- `cmd/simulator` — CLI + HTTP serve mode (the deployed manual job).
- `cmd/demosite` — renders `pages` layouts as real cross-origin browser pages.
- `cmd/gateway` — `/v1/sim/realism`, `/v1/sim/personas` (`sim.go`), demo persona/trace pages.
- `tests/e2e` — `harness/pages.go` + `harness/pages_world.go` replay `pages` layouts/sites; `external_pages_test.go` asserts per-format counts.

## Testing

`request/builder_test.go` + `pages/pages_test.go` are plain unit tests; `cmd/gateway/sim_test.go` is the web-UI/harness parity proof. Traffic generation against the live stack is the `/generate-data` skill.

## Pointers

- `docs/PLAN.md` → "Programmable Simulator" (CLI + Go library), "Seed Data and Simulation" / "Simulation Profiles", "Developer Tools" → "Publisher Simulator", "`pub-simulator` is a first-class seeded publisher"
- `docs/AUDIENCE-PIPELINE.md` — themed personas / earned-membership world
