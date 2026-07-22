# Diagrams — index & legend

The single source of truth for **what each diagram shows and when to update it**.
CLAUDE.md points here instead of carrying its own copy, so there's nothing to
keep in sync in two places.

## Tooling (two, no third)

- **D2** (`*.d2` → `*.svg`) for **structure** — "what connects to what". Render all
  with `make diagrams` (needs `d2` on PATH). Edit the `.d2`, never the `.svg`.
- **Mermaid** (fenced ` ```mermaid ` blocks in `.md`) for **flows/sequences** —
  "what happens in what order". Renders natively on GitHub; no tooling needed.

Rule of thumb: **D2 for maps, Mermaid for sequences.**

## Reading order (new here? start at the top)

1. **`context`** — the platform and the outside world (1 screen).
2. **`architecture`** — every service + store, grouped by plane (the reference map).
3. **flow diagrams** — pick the journey you care about (serving / money / data / async).
4. **`e2e-trace`** — one `trace_id` followed across *all* planes (the whole story).

## Index & "update when" legend

Status: ✅ current · ⚠️ stale (needs a refresh) · 🚧 planned (not built yet).

| Diagram | File | Level | Tool | Shows | **Update when** | Covers |
|---|---|---|---|---|---|---|
| Context | `context.d2` | L0 | D2 | platform box + external actors (browser/SDK, publisher, advertiser, competitor DSPs, S3) | ✅ an external integration is added/removed | boundary |
| Architecture | `architecture.d2` | L1 | D2 | all services + datastores + infra, grouped into the 5 planes | ✅ a **service, datastore, or service→service link** changes | `cmd/*`, `k8s/*` |
| Ad-request lifecycle | `auction-flow.d2` | flow | D2 | bid req → auction → win → serve → track | ✅ the serving/auction path changes | ssp, exchange, dsp, adserver, tracker |
| Async event fan-out | `nats-events.d2` | flow | D2 | NATS subjects → reporting/pipeline/billing/webhooks/identity | ✅ a **NATS subject or consumer** changes | `pkg/events`, consumers |
| Money loop | `billing-flow.d2` | flow | D2 | budget gate → win → impression → billing accrual → **TigerBeetle** → committed-spend snapshot → DSP reconcile | ✅ billing/budget/ledger flow changes | dsp budget, `pkg/billing`, reporting |
| Data & reporting | `data-reporting.md` | flow | Mermaid | event → **single write** (ClickHouse) → rollups → derived hourly **Parquet export** (cold) → **hot/cold store** → gateway → portal | ✅ a store, rollup, or read path changes | reporting, `pkg/store/*` |
| **E2E trace** ★ | `e2e-trace.md` | headline | Mermaid | **one `trace_id`, all planes**: request → auction → win (single source of truth) → serve → impression → {hot CH, cold Parquet export, billing→TB, budget reconcile} → report | ✅ any **new hop** in the request/event lifecycle | everything |
| End-to-end (subsystems) | `end-to-end-flow.md` | flow | Mermaid | per-subsystem sequences + standards overlays | ✅ a subsystem sequence changes | mixed |
| Cache freshness & feedback loops | `cache-freshness.d2` | flow | D2 | the two circulatory systems: serving caches feed from Postgres + NATS invalidates (never analytics); the analytics spine is read-side only EXCEPT four numbered feedback loops (memberships ≤30s, spend reconcile, boot warm-starts, identity) | ✅ a cache layer, preloader, invalidate subject, or feedback loop changes | warm caches, `pkg/audience/store/preload`, pacing reconcile, profile-builder |
| Targeting data flow | `targeting-data-flow.d2` | flow | D2 | observed (pixel/auction) + onboarded (1st/3rd party) sources → identity graph + ClickHouse → **profile-builder** → memberships → back into auctions + exports. Profile Store phases 1–5 SHIPPED | ✅ audience/identity/profile-store flow changes | ssp, tracker, identity-consumer, `cmd/pipeline`, `cmd/profile-builder`, `pkg/audience`, `pkg/profilebuilder` |
| **Data lifecycle (linear)** ★ | `data-lifecycle.md` | teaching | ASCII + Mermaid | the **single linear thread** stitching the data-flow diagrams: sources (1st/3rd-party + observed) → auction → NATS fork → {ClickHouse hot rollups · derived Parquet export · audience/profile-builder}. Start here to understand the data end-to-end | ✅ the data story spans multiple segments / onboarding a new reader | ssp, tracker, `cmd/pipeline`, `cmd/profile-builder`, reporting, `pkg/store/rollup` |

## Visual legend (same key in every D2 diagram)

So you can read any diagram without relearning it. Each `.d2` includes a small
`legend` container using these conventions:

**Shapes**
- `shape: person` — external actor (browser, publisher, advertiser)
- rectangle / container — a **service** (one per `cmd/*`)
- `shape: cylinder` — a **datastore** (Postgres, Redis, ClickHouse, Minio/lake, TigerBeetle)
- `shape: hexagon` — external system / infra (NATS, S3)

**Edges**
- **solid** `->` — synchronous call (HTTP / gRPC)
- **dashed** (`style.stroke-dash: 3`) — asynchronous (NATS event)
- **dotted** (`style.stroke-dash: 2`, lighter) — read / query

**Planes** (color bands / groups) — the 5 layers everything falls into:
| Plane | What lives here |
|---|---|
| 🟦 Serving (sync RTB) | ssp, exchange, dsp×3, adserver, publisher-adserver, tracker |
| 🟪 Async / events | NATS + reporting, pipeline, webhooks, identity-consumer |
| 🟩 Data / analytics | ClickHouse (hot + rollups), Parquet export (cold, via s3()), hot/cold store |
| 🟨 Money | budgets (Redis), `pkg/billing`, TigerBeetle, advertiser balances |
| ⬜ Control | gateway, portals, config, warm caches |

## Editing

1. Edit the `.d2` (or the ` ```mermaid ` block in the `.md`).
2. `make diagrams` to re-render SVGs (D2 only; Mermaid renders on GitHub).
3. Commit the `.d2`/`.md` **and** the regenerated `.svg`.
4. Check the **Update when** column above — if your change matches a trigger,
   that diagram must move in the same PR.
