# Clean Room Job Runner (NOT BUILT — placeholder)

**This directory is an empty scaffold** (`.gitkeep` from the Phase 1 repo layout).
No binary, no Dockerfile, no Helm entry, no routes/config keys — and no references
to fix. `pkg/cleanroom/` is likewise empty.

## Planned role

An **isolated K8s Job** (no network access) that runs multi-party clean-room
computations — overlap, expansion, composition — on hashed inputs, writing
aggregate-only results; raw data never leaves and is destroyed after the run.
See `docs/PLAN.md` -> "Clean Rooms and Data Marketplace".

## What shipped instead (clean-room-lite)

The Data Marketplace expansion estimate runs **in-process in the gateway**:
`POST /v1/api/marketplace/listings/{id}/estimate` (`cmd/gateway/marketplace.go`,
store in `pkg/marketplace`). Privacy enforced there: aggregate-only response +
`estimateMinAggregation = 100` floor (overlap under 100 users is suppressed).
The isolated-job boundary is a documented deferral — see the doc comment on
`marketplaceDoEstimate`.

## If you build it

Follow `cmd/CLAUDE.md` job conventions, keep the min-aggregation floor consistent
with the gateway estimate, add a Helm entry + `cmd/CLAUDE.md` row, and update
diagrams (`docs/diagrams/README.md`).
