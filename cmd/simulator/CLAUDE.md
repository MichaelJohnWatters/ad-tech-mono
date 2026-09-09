# Simulator (host CLI)

Production-like traffic generator. Every request is built from a Persona (geo,
device, identity, segments, consent regime) × a channel (display/video/audio/
native), so simulated traffic exercises the same exchange/DSP/targeting/privacy/
identity/fraud/deals paths as production — not a thin banner-only load gun.

## How it runs

Host-only, never deployed (no Dockerfile, no Helm entry). Subcommands:
`run`, `single`, `profiles`, `personas`, `check`. Entry points:
- `make traffic` — continuous steady profile (`DEMO_RPS`, default 5)
- `make loadtest RPS=100 DURATION=10m VERIFY=1 [USER_POOL=N]` — the perf path
  (full protocol = `/perf-loadtest` skill; don't improvise)
- `make demo` — seed + baseline traffic via `scripts/demo.sh`

Two modes:
- **Default (web-mirror, `serve.go`)** — each channel goes through the same
  first-party endpoint the web `/dev/publisher-simulator` tab uses (SSP serve
  for display; publisher-adserver VAST/audio/native otherwise) and fires the
  SERVER-returned signed beacons — byte-for-byte the production flow.
- **`--direct` (raw load)** — POSTs OpenRTB straight to the exchange and
  self-builds signed beacons via `pkg/adserving` with the real winner's
  campaign/creative/price and the trusted seat as `advid`.

Profiles: `trickle`, `steady`, `burst`, `themed` (themed adds the readable-world
personas; kept out of steady/burst so perf baselines keep their persona mix —
NB `simulator profiles` lists only the first 3; `listProfiles` hardcodes them).
`inventory.go` loads `profiles/publishers/*.yaml` (the same files `cmd/seed`
reads) so traffic spreads across every seeded publisher; themed personas land on
interest-matching placements ~80% of the time.

Rate model: a worker pool (`--concurrency`, default 2×rps floor 64 — a fixed
pool silently caps throughput at high `--rps`) fires in parallel; `--rps` caps
the AGGREGATE rate via a shared ticker; `--rps 0` = spam as fast as the backend
takes it.

## Key packages

- `pkg/simulator/request` — Persona registry + OpenRTB `Build` + `QueryParams`
  (shared source of truth with the web UI's realism params). Personas carry
  `HouseholdPool`: a fixed per-persona IP saturates per-household freq caps, so
  pooled personas spread over N addresses (`persona.go`).
- `pkg/adserving` — beacon URL building + HMAC signing (`--direct` + conversions)
- `pkg/routes`, `pkg/vast`, `pkg/tracing` (client traceparent per request)

## CRITICAL gotchas

- **Impression beacon = the money event.** Retried 3×; delivery means 2xx AND
  the `X-Adtech-Tracker: 1` response stamp — a bare 2xx once counted 492
  svclb-lost beacons as delivered and mislabelled client loss as pipeline
  slippage. Undelivered wins are reported as `Beacon-lost` for reconciliation.
- **User-id cardinality is deliberate.** Mirror path swaps the deterministic
  persona user_id for fresh random ids (else all volume collapses onto a few
  users and freq caps starve fill); themed personas use a small stable pool so
  min_count>1 rules are reachable; `--user-pool N` draws from the
  `synth-user-%06d` universe matching `cmd/seed --synthetic-users` so bid-time
  audience lookups HIT on density runs.
- **`--verify` invariants** (poll reporting up to 5m, quiescent stack only):
  imps ≥ client wins (no loss), imps ≤ server-recorded wins (no phantom
  billing), auctions == sent − errors, and server-computed `fill_rate` matches
  local counts. Exit non-zero on mismatch — usable as a CI gate.
- **Conversions are synthesised** (real ones happen off-platform): played for
  `ConvRate` of clicks, signed with the per-advertiser dev conversion key (G7
  strict validation rejects unsigned/wrong-key).
- Aborts if the first ~100 requests ALL fail — unseeded/reset world or dead
  stack; `make stack-doctor` / `make reset`, then rerun.
- `--requests N` without `--duration` removes the default 1m cap (N is the stop
  condition); an explicit `--duration` still caps.
- Stale Makefile targets: `simulate*`/`chaos*`/`ab-test*` omit the `run`
  subcommand and just print usage — use `traffic`/`loadtest`/`loadtest-ramp`.
- `pkg/simulator/pages` (demosite multi-slot page layouts) is consumed by
  `cmd/demosite` + `tests/e2e`, NOT by this CLI.

## Pointers

- `docs/PLAN.md` → "Seed Data and Simulation" (profiles, seed/simulator split,
  "Programmable Simulator"), "Developer Tools" → "Publisher Simulator",
  "`pub-simulator` is a first-class seeded publisher"
- Load-run protocol: `.claude/skills/perf-loadtest/SKILL.md`
