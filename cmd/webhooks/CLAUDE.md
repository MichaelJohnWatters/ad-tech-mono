# Webhooks Dispatcher Service

Consumes webhook events from NATS and delivers HTTP POST requests to registered webhook URLs.

## Responsibilities

- Consume from `adtech.webhooks.*` NATS subjects
- Look up registered webhooks for the event type and account
- Deliver HTTP POST with JSON payload to registered URLs
- HMAC-sign every payload for receiver verification
- Retry with exponential backoff on failure (3 attempts)
- Record delivery history (success/failure) in Postgres

## Key Packages Used

- `pkg/events/` - NATS consumption
- `pkg/store/postgres/` - webhook registrations, delivery history

## gRPC Services Exposed

- `WebhookService` - see `pkg/proto/`

## Webhook Events

See `docs/PLAN.md` -> "NATS Subjects" -> "Webhook Subjects" for full list.

## Dependencies

- NATS JetStream (consumes webhook events)
- Postgres (webhook registrations, delivery history)

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New webhook event type?** Update `docs/PLAN.md` -> Webhook Subjects table + NATS Event Flow diagram
- **New dependency?** Update `docs/diagrams/architecture.d2`
