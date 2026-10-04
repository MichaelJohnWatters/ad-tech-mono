# Tracker Service

Records every event in the ad lifecycle. Internet-facing - hit by end-user browsers via pixel URLs.

## Responsibilities

- Impression tracking (`/v1/t/imp`) - returns 1x1 pixel
- Click tracking (`/v1/t/click`) - 302 redirect to landing page
- Conversion tracking (`/v1/t/conv`) - returns 1x1 pixel
- Viewability tracking (`/v1/t/view`) - returns 204
- Validate request signatures (HMAC `sig` param)
- Real-time fraud checks before recording (bot UA, IP blocklist, rate limiting)
- Publish events to NATS JetStream
- Deduplication of events

## Key Packages Used

- `pkg/fraud/realtime.go` - real-time fraud middleware
- `pkg/events/` - NATS publishing
- `pkg/cache/redis/` - rate limiting counters, signature key cache

## HTTP Endpoints (External-Facing)

- `GET /v1/t/imp` - impression pixel
- `GET /v1/t/click` - click redirect
- `GET /v1/t/conv` - conversion pixel
- `GET /v1/t/view` - viewability beacon

## gRPC Services Exposed

- `TrackerService.RecordEvent` - internal services push events directly

## CRITICAL: Performance

These endpoints are the highest-volume in the system. They must:
- Respond in < 10ms
- Publish to NATS asynchronously (don't wait for ack before responding)
- Buffer events in-memory briefly if NATS is slow
- Flush buffer on graceful shutdown

## CRITICAL: Security

These endpoints are internet-facing. Every request must:
- Validate the `sig` HMAC signature
- Run through fraud checks (`pkg/fraud/realtime.go`)
- Never trust client-provided data for billing calculations

## Dependencies

- NATS JetStream (publishes all event types)
- Redis (rate limiting, signature key cache)

## Architecture Details

See `docs/PLAN.md` -> "Tracker Endpoints", "Viewability", "Real-Time Detection"

Routed directly via Traefik (bypasses Gateway for performance). See `docs/PLAN.md` -> "Ingress"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New tracking endpoint (e.g. /v1/t/newtype)?** Update `docs/PLAN.md` -> Tracker Endpoints, Traefik routing, NATS Subjects
- **New NATS event type published?** Update NATS Subjects table + NATS Event Flow diagram + Stream Design table
- **New fraud check added?** Update `docs/PLAN.md` -> Fraud Detection section
- **Changed Traefik routing?** update the C4 model (`docs/diagrams/workspace.dsl`) and run `make c4`
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
