# Notifications Service

In-app notification builder (:8096). The DB-row sibling of `cmd/webhooks`: consumes the same account-scoped business events, but the sink is a Postgres insert (the `notifications` table) instead of an HTTP POST. The gateway serves the portal bell + dropdown from that table.

## Responsibilities

- Consume account-scoped business events from NATS JetStream
- Translate each event payload → one per-account row (`pkg/notifications.Translate`)
- Insert durable `notifications` rows (migration 049) that back the portal bell
- Expose health/metrics only — the user-facing API lives in the gateway (`cmd/gateway/notifications.go`)

## Interfaces

### HTTP (health/metrics only)
- `GET /healthz`, `GET /readyz`, `GET /metrics` on `notifications.port` (default `routes.PortNotifications` = 8096). Readiness fails while Postgres or NATS is down.

### NATS subjects consumed (queue group `constants.NATSGroupNotifications`)
- `adtech.budget.depleted` → kind `budget_depleted`
- `adtech.balance.depleted` → kind `balance_depleted`
- `adtech.campaign.state_changed` → kind `campaign_state`
- `adtech.report.completed` → kind `report_ready`

Publishes nothing. Adding a notifiable event = one entry in `notificationSubjects` (main.go) + a case in `pkg/notifications/translate.go` + a new kind constant — no migration (kind is free text by design).

### Gateway-served API (not this binary)
- `GET/POST routes.APINotifications` (`/v1/api/notifications`) + `routes.APINotificationsRead` — JWT-gated list / unread-count / mark-read.

## Key Packages Used

- `pkg/notifications/` - Store interface + PostgresStore + Translate (the core; shared with the gateway's read side)
- `pkg/events/natsbus/` - JetStream subscribe with traceparent extraction
- `pkg/config/keys/notifications.go` - `notifications.port`, `notifications.nats_url`

## Dependencies

- Postgres (`notifications` table — RLS'd, tenant = the account whose portal shows the bell)
- NATS JetStream (event source; without it nothing is written)

## CRITICAL

- **1 replica** (helm `values.yaml` → `notifications.replicas: 1`). The consumer is queue-grouped, so extra replicas would load-balance (not duplicate) — but one is plenty and keeps ordering simple.
- **Boot-retry doctrine:** a Subscribe that fails at boot is retried every 15s until it sticks — never latch a dead consumer (fresh-stack 2026-07-26: every subject failed Subscribe once at boot and the table sat empty for 5h). Any new subject must go through the same `subscribeAll`/retry loop.
- **Ack/Nak discipline:** unmapped subject or payload with no `account_id` = poison → Ack (drop, warn). DB write failure = transient → Nak (redeliver). A Nak'd insert will run again — don't add side effects that can't tolerate a retry.
- **Tenant scoping:** `PostgresStore` sets the RLS GUC (`app.current_account_id`) in a tx AND filters by `account_id` explicitly. The service runs as `adtech_app` (NOBYPASSRLS) — a bare-pool query on `notifications` returns 0 rows.
- **Fail-soft boot:** Postgres/NATS unavailability degrades readiness, never crash-loops; the pod recovers when the dep does.

## Architecture Details

See `docs/PLAN.md` -> "NATS Subjects (Async Events)" (subject catalog) and "Webhooks" (the HTTP-delivery sibling consuming the same events).

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New NATS subject consumed?** Update `docs/PLAN.md` -> NATS Subjects table + NATS Event Flow diagram
- **New notification kind / gateway endpoint?** Update `docs/PLAN.md` -> Gateway HTTP Endpoints + `docs/openapi.yaml`
- **New dependency?** Update `docs/diagrams/architecture.d2` and run `make diagrams`
