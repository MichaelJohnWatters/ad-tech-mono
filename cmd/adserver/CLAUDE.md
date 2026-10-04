# Ad Server Service

Delivers ad creatives to end-user browsers. Generates tracking URLs with signed parameters.

## Responsibilities

- Store and serve creative assets (images, HTML, native)
- Generate impression, click, conversion, and viewability tracking URLs with HMAC signatures
- Handle ad fallback/default ads when no auction winner exists
- Frequency capping enforcement (Redis counters per user/campaign)
- Creative A/B rotation (even -> weighted -> winner)

## Key Packages Used

- `pkg/store/objects/` - object storage interface (filesystem / S3)
- `pkg/cache/redis/` - frequency cap counters, creative metadata cache
- `pkg/models/` - Creative model

## gRPC Services Exposed

- `InternalAdServeService.Serve` (:8185) - see `pkg/proto/internalrpc/`;
  JSON-envelope twin of `POST /v1/ad/serve`, dialled only by our SSP. The
  429 frequency-cap decline rides the envelope's HTTP-equivalent status.

## Dependencies

- Object storage (creative assets - filesystem locally, S3 in prod)
- Redis (frequency cap counters, creative metadata L2 cache)
- Tracker (indirect: bakes signed tracker pixel URLs into served HTML; the browser fires them)

## Architecture Details

See `docs/PLAN.md` -> "Ad Server", "Creative Review and Approval", "Viewability"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New creative format supported?** Update `docs/PLAN.md` -> relevant format section + Creative Review
- **New gRPC method?** Update gRPC Services + Gateway HTTP Endpoints
- **Changed macro list?** Update `docs/PLAN.md` -> Macro Substitution table
- **New dependency?** update the C4 model (`docs/diagrams/workspace.dsl`) and run `make c4`
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
