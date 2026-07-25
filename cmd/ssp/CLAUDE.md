# SSP Service

The supply-side platform. Publishers' inventory lives here. Generates bid requests when ad slots need filling.

## Responsibilities

- Publisher and placement management (CRUD via gRPC from Gateway)
- Generate bid requests with placement details, user signals, publisher first-party data
- Send bid requests to Exchange via the internal gRPC twin (`grpc://` scheme in `ssp.exchange_url`; `http://` falls back to OpenRTB HTTP)
- Call the Ad Server's gRPC twin to render the winner (`ssp.adserver_url`)
- Manage publisher quality controls (blocklists, allowlists, category filters)
- Floor price management (static, time-based, device-based, geo-based)
- Generate ad tags for publishers to embed on their sites

## Key Packages Used

- `pkg/models/` - Publisher, Placement models
- `pkg/deals/` - deal configuration per placement
- `pkg/store/postgres/` - placement CRUD (with multi-tenancy)
- `pkg/cache/` - L1 placement configs

## gRPC Services Exposed

- None. The SSP is a gRPC *client* only (exchange + ad server twins in
  `pkg/proto/internalrpc/` via `pkg/grpcx`); its own endpoints are HTTP
  (publisher ad tags are browser-facing).

## Dependencies

- Postgres (publishers, placements, quality controls, deals)
- Exchange (internal gRPC twin — or OpenRTB HTTP when `ssp.exchange_url` is `http://`)
- Ad Server (internal gRPC twin — or HTTP when `ssp.adserver_url` is `http://`)

## Architecture Details

See `docs/PLAN.md` -> "Supply-Side Platform", "Publisher Workflows", "Floor Price Management"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New gRPC method?** Update `docs/PLAN.md` -> gRPC Services + Gateway HTTP Endpoints
- **Changed bid request format?** Update `docs/PLAN.md` -> OpenRTB sections + Ad Request Flow sequence diagram
- **New dependency?** Update `docs/diagrams/architecture.d2`
