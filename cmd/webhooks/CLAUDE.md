# Webhooks Dispatcher Service

Consumes account-scoped business events from NATS and delivers HMAC-signed HTTP
POSTs to customer-registered webhook URLs (the `webhooks` table). SHIPPED
2026-07-05. Runs as a k8s pod (port 8091) — a background consumer with no
external ingress, exposing only `/healthz`, `/readyz`, `/metrics`.

## How it works

- `pkg/webhooks.Dispatcher` is the testable core: given an `Event` (type +
  account_id + raw JSON), it looks up the account's active subscriptions for
  that event type, wraps the raw event in a self-describing envelope, and
  delivers to each endpoint with retries.
- `cmd/webhooks/main.go` maps NATS subjects → customer-facing event names
  (`eventRoutes`), extracts `account_id` from each event, and dispatches.

## Event catalog (subject → event name)

Every mapped event payload carries an `account_id`. The event name is the string
a customer puts in a subscription's `events` list (`POST /v1/api/webhooks`).

| NATS subject | Event name | Fires when |
|---|---|---|
| `adtech.budget.depleted` | `budget.depleted` | a campaign exhausts its budget |
| `adtech.balance.depleted` | `balance.depleted` | an advertiser's prepay balance hits zero |
| `adtech.campaign.state_changed` | `campaign.state_changed` | a campaign is paused/resumed/etc. |
| `adtech.retargeting.enrolled` | `retargeting.enrolled` | a shopper is enrolled into a real-time retargeting audience (abandoned-cart push hook) |

Add a new event = one entry in `eventRoutes` + a row here. The source payload
must carry `account_id`.

## Delivery contract (what receivers get)

- `POST` with JSON body `{event, account_id, timestamp, data}` where `data` is
  the raw originating event.
- Headers: `X-Adtech-Signature: sha256=<hmac>` (over the raw body, keyed by the
  subscription secret), `X-Adtech-Event`, `X-Adtech-Timestamp`, `X-Adtech-Delivery`.
- Success = HTTP 2xx. Otherwise retried up to `webhooks.max_attempts` (default 3)
  with exponential backoff (`webhooks.backoff_base`). Every attempt is written to
  `webhook_deliveries`.
- At-least-once: keep `max_attempts × http_timeout + backoff` under the NATS
  AckWait (30s) to avoid NATS-level redelivery on top of our own retries.
  Receivers should dedup on `X-Adtech-Delivery` + signature.

## Key packages / tables

- `pkg/webhooks/` - Dispatcher + PostgresStore (the `webhooks` +
  `webhook_deliveries` tables; migration 014). Subscription CRUD lives in the
  gateway (`cmd/gateway/webhooks.go`, `POST/GET/DELETE /v1/api/webhooks`).
- `pkg/events/` - NATS consumption (group `constants.NATSGroupWebhooks`).

## Dependencies

- NATS JetStream (event source; readiness fails without it)
- Postgres (subscription source + delivery log)

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New webhook event type?** Update the event catalog table above + NATS Event Flow diagram
- **New dependency?** Update `docs/diagrams/architecture.d2`
