# Gateway Service

Single entry point for all external traffic (except Tracker and Exchange hot paths which Traefik routes directly).

## Responsibilities

- Authentication: JWT issuance, validation, refresh
- Authorisation: RBAC checks before proxying
- Multi-tenancy: extracts account_id from JWT, injects into gRPC metadata
- REST API: translates HTTP/JSON to gRPC calls to internal services
- HTMX Dashboard: serves Go HTML templates for the UI
- Swagger UI: serves OpenAPI docs at `/docs`
- sellers.json: auto-generated at `/sellers.json`
- adtech.js SDK: versioned serving at `/sdk/` (#106) — public, no auth. `pkg/sdkasset`
  parses the SDK's own `SDK_VERSION` at boot → serves `/sdk/<exact>/adtech.js`
  (immutable 1y, SRI-safe), `/sdk/v<major>/adtech.js` (1h, patched, no SRI),
  `/sdk/latest/adtech.js` (short), + `/sdk/version.json` metadata (version, sha384
  integrity, urls). ACAO:* on all. Unhosted version → 404. `/static/adtech.js` still
  served for old embeds; the ad-tag generator now emits the `/sdk/v<major>/` URL.
  Served as bare literals (like `/static/`), not `pkg/routes` consts.
- Rate limiting: per-account, per-API-key

## Key Packages Used

- `pkg/middleware/` - auth, tenant, rate limiting, audit
- `pkg/email/` - sends verification, password reset, notification emails
- `pkg/config/` - serves live config UI

## What It Proxies

All `/v1/api/*` endpoints proxy to internal gRPC services. See `docs/openapi.yaml` for full endpoint list.

| API path | Proxies to |
|---|---|
| `/v1/api/campaigns/*` | DSP CampaignService |
| `/v1/api/creatives/*` | Ad Server AdService |
| `/v1/api/publishers/*` | SSP InventoryService |
| `/v1/api/reports/*` | Reporting ReportingService |
| `/v1/api/billing/*` | Billing BillingService |
| `/v1/api/config/*` | ConfigService (local to Gateway) |
| `/v1/api/webhooks/*` | Webhooks WebhookService |
| `/v1/api/ops/*` | Operations (A/B tests, canary, deployments) |
| `/v1/api/moderation/*` | Platform moderation queues |

## Custom Dockerfile

Uses `build/Dockerfile.gateway` because it embeds `web/templates/` and `web/static/`.

## Dependencies

- All internal services (via gRPC)
- Postgres (accounts, sessions, config table, audit log)
- Redis (JWT sessions, rate limit counters)

## Architecture Details

See `docs/PLAN.md` -> "Gateway", "Signup and Onboarding", "API Access Tiers"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New API endpoint group?** Update `docs/PLAN.md` -> Gateway HTTP Endpoints (consolidated) + OpenAPI spec
- **New gRPC proxy target?** Update `docs/diagrams/architecture.d2` (Gateway -> service connection)
- **Changed auth/RBAC?** Update `docs/PLAN.md` -> Authentication, Roles, and Permissions
- **New Traefik route?** Update `docs/diagrams/architecture.d2` ingress section
