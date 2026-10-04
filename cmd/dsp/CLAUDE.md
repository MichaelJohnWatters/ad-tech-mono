# DSP Service

The demand-side platform. Advertisers' campaigns live here. Evaluates bid requests and decides whether/how much to bid.

## Responsibilities

- Manage campaigns (CRUD via gRPC from Gateway)
- Evaluate incoming bid requests against active campaigns and targeting rules
- Calculate bid price based on campaign strategy (CPM, CPC, CPA)
- Budget pacing: spread spend evenly across campaign flight
- Publish `BudgetDepletedEvent` when a campaign runs out of budget

### Budget accounting (two meters, reconciled)

The DSP does **not** run its own reserve/settle/release state machine. Spend
accounting is single-sourced in the billing engine (`cmd/reporting` / `pkg/billing`),
and the DSP mirrors it:

1. **Local win-notice counter** — `winHandler` (`/v1/openrtb/win` nurl) does
   `BudgetTracker.Record`, incrementing `dsp:budget:{campaign}:spent` in Redis on every
   win. Fast and immediate, but a deliberate **over-count**: it counts phantom wins that
   never impress and the full clearing price on CPC/CPA where only the settle bills. This
   is the intra-snapshot overspend guard.
2. **Reconcile to billed reality** — the billing engine computes per-campaign *committed*
   spend (settled + open reserves), and reporting broadcasts it every
   `reporting.spend_snapshot_interval` on `adtech.billing.campaign_spend_snapshot`. The DSP
   consumes it (`pacing_reconcile.go`, fan-out per pod) and `BudgetTracker.Reconcile`
   overwrites the counter to the authoritative value — releasing phantom wins and
   correcting the CPC/CPA over-count. Gated by `dsp.spend_reconcile_enabled`.

Net effect: between snapshots pacing is conservative (won't overspend); on each snapshot it
snaps to what actually bills. Reserve/settle/release semantics live in `pkg/billing`, where
the CPC/CPA/viewability rates are known — not duplicated here.

## Key Packages Used

- `pkg/targeting/` - evaluates if a bid request matches campaign targeting
- `pkg/pacing/` - budget pacing calculations
- `pkg/models/` - Campaign, Creative, Targeting models
- `pkg/cache/` - L1 campaign configs, L2 Redis budget counters
- `pkg/events/` - NATS publishing/consuming
- `pkg/store/postgres/` - campaign CRUD (with multi-tenancy)

## gRPC Services Exposed

- `InternalBidService.Bid` (:8182) - see `pkg/proto/internalrpc/`;
  JSON-envelope twin of `POST /v1/openrtb/bid`, dialled only by OUR exchange
  (competitor-profile pods listen too but are always called over HTTP)

## OpenRTB Endpoint

- `POST /v1/openrtb/bid` - receives bid requests from Exchange, responds with bid or no-bid; same handler backs the gRPC twin via `pkg/grpcx.Bridge`

## Budget Flow

See `docs/PLAN.md` -> "Budget Handling", "How Billing Models Interact with AuctionWinEvent"

## Dependencies

- Postgres (campaigns, targeting rules, budgets)
- Redis (budget counters, campaign config L2 cache)
- NATS (consumes `adtech.billing.campaign_spend_snapshot` for pacing reconcile + cache-invalidate subjects; publishes BudgetDepletedEvent / BalanceDepletedEvent). Win notices arrive over HTTP (the OpenRTB nurl), not NATS.
- Exchange calls this service via the internal gRPC twin (our profile) or OpenRTB HTTP (competitor profiles / rollback)

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New gRPC method?** Update `docs/PLAN.md` -> gRPC Services + Gateway HTTP Endpoints
- **New NATS subject?** Update `docs/PLAN.md` -> NATS Subjects table + NATS Event Flow diagram
- **Changed bid evaluation pipeline (pacing/targeting/shading order)?** Update `docs/PLAN.md` -> Ad Request Flow sequence diagram
- **New dependency?** update the C4 model (`docs/diagrams/workspace.dsl`) and run `make c4`
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
