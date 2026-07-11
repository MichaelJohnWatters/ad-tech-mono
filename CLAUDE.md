# Ad Tech Mono - AI Context

## Git Workflow (IMPORTANT)

**Always work on `main`.** Commit and push directly to `main` — do NOT create
feature branches for this project. This overrides the default "branch first"
behaviour; feature branches here just create stale, confusing duplicates. When
asked to commit or push, do it on `main`.

## What This Is

A full-stack programmatic advertising platform in a single Go monorepo. Every component - from bid request to impression tracking - runs locally on K8s (Colima + k3s). The goal is full transparency: trace any ad request end-to-end with zero data slippage.

See `docs/PLAN.md` for the comprehensive project plan.

## Tech Stack

- **Language:** Go everywhere (backend, frontend, tooling). Python only for ML/data science.
- **Frontend:** Go templates + HTMX + Tailwind CSS. No JS build pipeline.
- **Protocols:** gRPC (internal), OpenRTB JSON/HTTP (bidding), HTTP/JSON (dashboard), NATS JetStream (async events)
- **Databases:** PostgreSQL (transactional), DuckDB/ClickHouse (analytics), Parquet + Delta Log (data pipeline)
- **Object storage:** S3 everywhere - Minio locally, real S3 in staging/prod. One code path.
- **Caching:** L1 in-process (Go maps) + L2 Redis + L3 Postgres
- **Infrastructure:** K8s everywhere (Colima + k3s local, k3s prod), Kustomize overlays (local/staging/prod), Tilt for dev orchestration
- **Observability:** slog (logging), Prometheus + Grafana (metrics), Loki (log aggregation), Jaeger (tracing)

## Monorepo Layout

- `cmd/` - service entrypoints (one per service/job)
- `pkg/` - shared packages (all reusable libraries, nothing external)
- `web/` - HTML templates, static assets, adtech.js SDK
- `k8s/` - K8s manifests (base + overlays per environment)
- `build/` - Dockerfiles
- `migrations/` - goose SQL migration files
- `profiles/` - seed data, simulation, publisher configs, fraud rules
- `docs/` - PLAN.md (architecture), openapi.yaml (REST API spec)
- `python/` - ML model training (fraud, optimisation)

## Coding Conventions

### Multi-Tenancy (CRITICAL)
- Every store method MUST read `account_id` from context via `auth.AccountIDFromContext(ctx)`
- Never create a query without tenant filtering
- Postgres RLS is the safety net but code must still filter explicitly
- See `docs/PLAN.md` -> "Multi-Tenancy Isolation"

### Trace IDs
- Every request gets a trace ID at the SSP
- Propagate via `X-Trace-ID` HTTP header and gRPC metadata
- Every slog call must include `trace_id` field
- Every NATS message must include `trace_id`

### Event Bus
- Services never import NATS directly - use `pkg/events/` interface
- All NATS messages are protobuf-encoded
- New subjects must be added to `docs/PLAN.md` -> "NATS Subjects"

### Protobuf
- All `.proto` files live in `pkg/proto/`
- `schema_version` is always field 1 in every message
- Generate code via `make proto` (uses Buf)
- Proto comments become API documentation

### Database
- Migrations use goose in `migrations/*.sql`
- Every new table needs an RLS policy (see migration 029)
- Parameterised queries only - no string concatenation
- Store interfaces in `pkg/store/`, implementations in subdirectories

### API
- All REST endpoints use `/v1/` prefix
- OpenRTB endpoints: `/v1/openrtb/`
- Tracker endpoints: `/v1/t/`
- Dashboard API: `/v1/api/`
- See `docs/openapi.yaml` for full endpoint spec

### Caching
- L1 (in-process) for read-heavy config data
- L2 (Redis) for shared mutable state (budgets, frequency caps)
- Cache invalidation via Core NATS pub/sub (`adtech.cache.invalidate.*`)
- Redis keys prefixed by service name: `dsp:budget:{id}`, `adserver:freqcap:{id}`

### Privacy
- Never store raw PII - hash client-side before transmission
- Consent signal must flow through entire chain
- No user-level targeting without consent
- See `pkg/privacy/`

### Services
- Every service exposes `/healthz` (liveness) and `/readyz` (readiness)
- Every service uses `pkg/lifecycle/` for graceful shutdown
- Every service uses `pkg/logger/` for structured JSON logging
- Every service loads config via `pkg/config/` (defaults -> env vars -> live config)

### Testing
- Unit tests: no mocks for databases. Use interfaces + fakes in `pkg/testutil/`
- Integration tests: use testcontainers-go for real Postgres/Redis/NATS
- Never mock Postgres, Redis, NATS, or filesystem
- Tag integration tests with `-tags=integration`

## Key Design Decisions

| Decision | Reference |
|---|---|
| Campaign hierarchy (IO > Line Item > Creative) | `docs/PLAN.md` -> "Campaign Hierarchy" |
| First-price auction default + bid shading | `docs/PLAN.md` -> "First-Price Auction", "Bid Shading" |
| Single source of truth for cost | `docs/PLAN.md` -> "Single Source of Truth: The AuctionWinEvent" |
| Budget reserve/settle for CPC/CPA/vCPM | `docs/PLAN.md` -> "How Billing Models Interact with AuctionWinEvent" |
| Deal priority order (PG > Preferred > PMP > Open) | `docs/PLAN.md` -> "Deal Management" |
| Fraud scoring approach | `docs/PLAN.md` -> "Fraud Detection and Traffic Quality" |
| Identity graph architecture | `docs/PLAN.md` -> "Identity and First-Party Data" |
| User opt-out system (3 levels) | `docs/PLAN.md` -> "User Opt-Out and Data Deletion System" |
| Data rollup chain (universal framework) | `docs/PLAN.md` -> "Data Rollups", "Universal Rollup Framework" |
| Cache invalidation pattern | `docs/PLAN.md` -> "Cache Invalidation" |
| Variable margin/revenue share per publisher | `docs/PLAN.md` -> "Variable Margin and Revenue Share" |
| Video/SSAI/CTV architecture | `docs/PLAN.md` -> "Video Ads, SSAI, and CTV" |
| Developer tools (Trace Explorer, Publisher Simulator) | `docs/PLAN.md` -> "Developer Tools" |
| `campaign_id` = line item ID everywhere | `docs/PLAN.md` -> "Campaign Hierarchy" (note at bottom) |

## Working On a Specific Area?

| Area | Read first |
|---|---|
| A service (`cmd/*`) | That service's `CLAUDE.md` |
| A shared package (`pkg/*`) | That package's `CLAUDE.md` |
| K8s / infrastructure | `k8s/CLAUDE.md` |
| Database changes | `migrations/CLAUDE.md` |
| API changes | `docs/openapi.yaml` + relevant service CLAUDE.md |
| Adding a NATS subject | `docs/PLAN.md` -> "NATS Subjects" section |
| Diagrams | `docs/diagrams/` - D2 source files, `make diagrams` to regenerate SVGs |

## When to Update Diagrams

After making changes, check if any diagrams need updating:

| Change type | Diagram to check |
|---|---|
| New service added | `docs/diagrams/architecture.d2` - add the service and its connections |
| New NATS subject | `docs/PLAN.md` -> NATS Event Flow mermaid diagram |
| New database table | `docs/PLAN.md` -> ER diagram |
| Service-to-service connection changed | `docs/diagrams/architecture.d2` + sequence diagram in PLAN.md |
| New infrastructure component | `docs/diagrams/architecture.d2` + `k8s/CLAUDE.md` |
| New API endpoint group | Check if Gateway routing diagram needs updating |
| Traefik routing changed | `docs/diagrams/architecture.d2` - ingress section |

**Rule: if your PR changes how services connect, it MUST include a diagram update.** CI can check if `.d2` files are modified when `cmd/` or `k8s/` files change.
