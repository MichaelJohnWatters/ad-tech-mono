# Exchange Service

The central auction marketplace. Receives bid requests from SSPs, fans out to DSPs, runs the auction, selects a winner.

## Responsibilities

- Receive bid requests from SSP via gRPC (`AuctionService.RunAuction`)
- Evaluate deal priority: PG -> Preferred Deal -> PMP -> Open Auction
- Fan out bid requests to eligible DSPs via OpenRTB JSON/HTTP
- Enforce bid timeout (configurable via live config: `exchange.bid_timeout_ms`)
- Run auction (second-price by default)
- Publish `AuctionWinEvent` to NATS (single source of truth for cost)
- Publish `AuctionCompleteEvent` to NATS (all bids, winner, timing)
- Send win/loss notifications to DSPs
- Validate ads.txt before accepting bid requests

## Key Files (when they exist)

- `main.go` - service entrypoint
- Uses `pkg/auction/` for auction logic
- Uses `pkg/auction/router.go` for smart DSP fan-out (phases 1-3)
- Uses `pkg/deals/` for deal matching and priority
- Uses `pkg/fraud/adstxt.go` for ads.txt verification
- Uses `pkg/events/` for NATS publishing

## gRPC Services Exposed

- `AuctionService.RunAuction` - see `pkg/proto/`

## OpenRTB Endpoints (HTTP)

- `POST /v1/openrtb/auction` - receives bid requests from SSP
- `POST /v1/openrtb/bid` - sends bid requests to DSPs
- `GET /v1/openrtb/win` - win notification to DSP
- `GET /v1/openrtb/loss` - loss notification to DSP

## Dependencies

- NATS JetStream (publishes auction events)
- DSP services (calls via OpenRTB HTTP)
- Ad Server (calls via gRPC after auction win)
- Redis (L1 cache for DSP endpoints, floor prices, ads.txt)
- Postgres (deal configs, publisher quality controls)

## Architecture Details

See `docs/PLAN.md` -> "Ad Exchange", "Deal Management", "Smart Routing", "ads.txt"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New gRPC method or endpoint?** Update `docs/PLAN.md` -> gRPC Services section and Gateway HTTP Endpoints
- **New NATS subject published?** Update `docs/PLAN.md` -> NATS Subjects table and NATS Event Flow diagram
- **New dependency (Redis, Postgres, another service)?** Update `docs/diagrams/architecture.d2` and run `make diagrams`
- **Changed auction strategy or channel routing?** Update `docs/PLAN.md` -> Unified Auction Engine section
