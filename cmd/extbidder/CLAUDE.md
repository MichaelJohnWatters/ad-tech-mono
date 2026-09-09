# extbidder — External DSP Partner (host tool)

A standalone simulated competitor DSP that runs OUTSIDE the cluster and speaks
only OpenRTB JSON/HTTP. The exchange fans out to it via `exchange.dsp_endpoints`,
exercising the real outbound path: cross-network OpenRTB, the auction timeout
budget, a bidder we don't control. Demand-side mirror of `cmd/demosite`.

## How it runs

- `make extbidder` / `go run ./cmd/extbidder` — host process on `:9100`
  (`EXTBIDDER_PORT`). Never deployed by the Helm chart; for a separate-cluster
  run build the generic image (`build/Dockerfile --build-arg SERVICE=extbidder`).
- Wire in: append `http://host.docker.internal:9100` to the
  `exchange.dsp_endpoints` live-config row (picked up on the next config poll).
  Full wiring + ClickHouse verification queries: `cmd/extbidder/README.md`
  (env-var table too — seat/brand/markup/no-bid-rate/media URLs).

## Interfaces (HTTP only — no gRPC, no NATS)

- `POST /v1/openrtb/bid` (`routes.OpenRTBBid`) — bids `floor × (1 + markup)`
  ($1 CPM assumed when no floor), no-bids `EXTBIDDER_NOBID_RATE` of requests,
  echoes `imp.DealID`, returns a renderable "EXTERNAL DSP"-branded creative
  per format (HTML banner / VAST video / DAAST audio / native 1.2 JSON).
  Applies `EXTBIDDER_AUDIENCE_UPLIFT` extra markup when the request carries
  standard-taxonomy audience data (`user.data` with `ext.segtax`).
- `GET /v1/debug/user-data[?id=<bidreq-id>]` — last 100 `user.data` payloads
  received (recorded even on no-bid); manual proof that segtax audience data
  crossed the network boundary to an external buyer.
- `/healthz`, `/readyz` — always ok (no dependencies).

## CRITICAL

- **External boundary means OpenRTB HTTP, never internal gRPC** — this binary
  has no gRPC server; the exchange dials it via an `http://` URL. Keep it that
  way: it models a third party we own neither end of.
- **Deliberately self-contained** — no Postgres/Redis/NATS, no `pkg/config` /
  `pkg/logger` / `pkg/lifecycle`. The `cmd/` shared-package conventions do NOT
  apply here (only `pkg/routes` + `pkg/openrtb` for wire compatibility). Don't
  "fix" it by wiring in platform packages.
- **Remove `host.docker.internal:9100` from `exchange.dsp_endpoints` when the
  bidder isn't running** — a dead endpoint wastes the auction timeout on every
  fan-out.
- The automated segtax e2e (`tests/e2e/segtax_test.go`) uses
  `harness.FakeDSP` over the same host-network path, not this binary — this one
  is for demos/manual runs.

## Pointers

- `docs/PLAN.md` -> "External DSP Partners (Multi-Tenant Integration Platform)"
- `docs/PLAN.md` -> "Exchange Fan-Out: Latency, Timeouts, and Adaptive Routing"
  + "Smart Routing (Exchange DSP Fan-Out)" (how the exchange treats a slow/dead
  external endpoint)
- `docs/PLAN.md` -> "OpenRTB Endpoints (Bidding - JSON/HTTP)"
