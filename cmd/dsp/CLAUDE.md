# DSP Service

The demand-side platform. Advertisers' campaigns live here. Evaluates bid requests and decides whether/how much to bid.

## Responsibilities

- Manage campaigns (CRUD via gRPC from Gateway)
- Evaluate incoming bid requests against active campaigns and targeting rules
- Calculate bid price based on campaign strategy (CPM, CPC, CPA)
- Budget management: reservation on win, settle on click/conversion, release on expiry
- Budget pacing: spread spend evenly across campaign flight
- Publish `BudgetDepletedEvent` when a campaign runs out of budget
- Consume `AuctionWinEvent` from NATS: decrement budget in Redis

## Key Packages Used

- `pkg/targeting/` - evaluates if a bid request matches campaign targeting
- `pkg/pacing/` - budget pacing calculations
- `pkg/models/` - Campaign, Creative, Targeting models
- `pkg/cache/` - L1 campaign configs, L2 Redis budget counters
- `pkg/events/` - NATS publishing/consuming
- `pkg/store/postgres/` - campaign CRUD (with multi-tenancy)

## gRPC Services Exposed

- `CampaignService` - see `pkg/proto/`

## OpenRTB Endpoint

- `POST /v1/openrtb/bid` - receives bid requests from Exchange, responds with bid or no-bid

## Budget Flow

See `docs/PLAN.md` -> "Budget Handling", "How Billing Models Interact with AuctionWinEvent"

## Dependencies

- Postgres (campaigns, targeting rules, budgets)
- Redis (budget counters, campaign config L2 cache)
- NATS (consumes AuctionWinEvents, publishes BudgetDepletedEvent)
- Exchange calls this service via OpenRTB HTTP

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New gRPC method?** Update `docs/PLAN.md` -> gRPC Services + Gateway HTTP Endpoints
- **New NATS subject?** Update `docs/PLAN.md` -> NATS Subjects table + NATS Event Flow diagram
- **Changed bid evaluation pipeline (pacing/targeting/shading order)?** Update `docs/PLAN.md` -> Ad Request Flow sequence diagram
- **New dependency?** Update `docs/diagrams/architecture.d2` and run `make diagrams`
