# SSP Service

The supply-side platform. Publishers' inventory lives here. Generates bid requests when ad slots need filling.

## Responsibilities

- Publisher and placement management (CRUD via gRPC from Gateway)
- Generate bid requests with placement details, user signals, publisher first-party data
- Send bid requests to Exchange via gRPC
- Manage publisher quality controls (blocklists, allowlists, category filters)
- Floor price management (static, time-based, device-based, geo-based)
- Generate ad tags for publishers to embed on their sites

## Key Packages Used

- `pkg/models/` - Publisher, Placement models
- `pkg/deals/` - deal configuration per placement
- `pkg/store/postgres/` - placement CRUD (with multi-tenancy)
- `pkg/cache/` - L1 placement configs

## gRPC Services Exposed

- `InventoryService` - see `pkg/proto/`

## Dependencies

- Postgres (publishers, placements, quality controls, deals)
- Exchange (calls via gRPC to initiate auctions)

## Architecture Details

See `docs/PLAN.md` -> "Supply-Side Platform", "Publisher Workflows", "Floor Price Management"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New gRPC method?** Update `docs/PLAN.md` -> gRPC Services + Gateway HTTP Endpoints
- **Changed bid request format?** Update `docs/PLAN.md` -> OpenRTB sections + Ad Request Flow sequence diagram
- **New dependency?** Update `docs/diagrams/architecture.d2`
