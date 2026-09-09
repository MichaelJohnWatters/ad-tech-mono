# Ad Tech Mono - AI Context

## Git Workflow (IMPORTANT)

**Always work on `main`.** Commit and push directly to `main` — do NOT create
feature branches for this project. This overrides the default "branch first"
behaviour; feature branches here just create stale, confusing duplicates. When
asked to commit or push, do it on `main`.

## What This Is

A full-stack programmatic advertising platform in a single Go monorepo. Every component - from bid request to impression tracking - runs locally on K8s (Rancher Desktop k3s, deployed via the Helm chart). The goal is full transparency: trace any ad request end-to-end with zero data slippage.

See `docs/PLAN.md` for the comprehensive project plan.

## Tech Stack

- **Language:** Go everywhere (backend, frontend, tooling). Python only for ML/data science.
- **Frontend:** Go templates + HTMX + Tailwind CSS. No JS build pipeline.
- **Protocols:** gRPC on internal hot-path edges where we own BOTH ends (SSP→Exchange, Exchange→our DSP, SSP→AdServer; `pkg/grpcx`, transport picked by `grpc://` vs `http://` URL scheme in config), OpenRTB JSON/HTTP on every external bidding boundary (third-party DSPs, Prebid, win/loss), HTTP/JSON (dashboard + gateway proxy), NATS JetStream (async events, JSON payloads)
- **Databases:** PostgreSQL (transactional), DuckDB/ClickHouse (analytics), Parquet + Delta Log (data pipeline)
- **Object storage:** S3 everywhere - Minio locally, real S3 in staging/prod. One code path.
- **Caching:** L1 in-process (Go maps) + L2 Redis + L3 Postgres
- **Infrastructure:** K8s everywhere (Rancher Desktop k3s local, k3s prod), Helm chart `k8s/helm/adtech` (per-env values files), `make stack-up` / `make deploy SVC=x` dev loop (Tilt + kustomize retired 2026-07-18 — see k8s/CLAUDE.md)
- **Local deploy + seed + reset:** the full command map (deploy · migrate · `make seed`/`demo`/`reset` · the `/dev/reset-and-reseed` API · API-vs-DB-direct seeding · external demo origins) lives in **`k8s/CLAUDE.md` → "Dev loop (Makefile)" / "Seed / reset / demo data"**. Quick refs: `make stack-up` (deploy+migrate), `make demo` (seed+traffic), `make reset` (wipe all 3 stores + reseed), `make deploy SVC=x` (rebuild one service).
- **Observability:** slog (logging), Prometheus + Grafana (metrics), Loki (log aggregation), Jaeger (tracing)

## Monorepo Layout

- `cmd/` - service entrypoints (one per service/job)
- `pkg/` - shared packages (all reusable libraries, nothing external)
- `web/` - HTML templates, static assets, adtech.js SDK
- `k8s/` - Helm chart (`helm/adtech`, per-env values) + frozen pre-Helm manifests (`base/`, parity reference)
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
- NATS payloads are JSON (`pkg/events` `PublishJSON`/`PublishJSONID`; typed payload structs in `pkg/events/payloads.go`) — not protobuf
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
- **Always consider an e2e test.** Any new feature, endpoint, flow, or fix that
  spans services (a new API + its behaviour, a portal action, an auth/RBAC gate, a
  money/event/cache path) gets an e2e in `tests/e2e/*` (`-tags=e2e`) that exercises
  it against the LIVE stack — build → `make deploy SVC=x` → run the e2e — not just
  unit tests. Unit-test the pure logic too (validators, state machines, scoring),
  but the e2e is what proves it actually works end-to-end. If a change genuinely
  can't be e2e'd (a documented mock boundary, an external-only surface), say so
  explicitly rather than skipping silently. See `tests/e2e/partners_test.go` for
  the per-slice shape (register → act → assert persistence + the negative/RBAC case).
- **Always consider the enforcement surface.** For every new endpoint / feature /
  flow, deliberately work through which enforcements apply and cover them — don't
  ship an unguarded surface:
  - **Authentication** — is it behind `authMiddleware` / a valid session/API key? Or
    deliberately public (say why)?
  - **Authorization (RBAC)** — the right `resource:action` permission + account
    type; a wrong-role/wrong-type caller gets 403.
  - **Tenant isolation** — reads/writes scoped to `account_id` (RLS + explicit
    filter); platform-global tables justified; cross-tenant only via the platform
    hatch. A bare-pool query on an RLS table under `adtech_app` returns 0 rows.
  - **Signing / HMAC** — beacons/postbacks/URLs that can be forged are signed
    (`ValidateSignatureAny`); note strict-vs-warn mode.
  - **Trusted vs self-declared** — bill/attribute on the value WE control (e.g. the
    endpoint-bound seat), never the caller's self-declared field.
  - **Consent / privacy** — personalisation gated on `privacy.Evaluate().Personalise`
    where user data is involved.
  - **Input safety** — body caps, id/UUID validation, client-error → 4xx not 500.
  The catalog + an omission-focused audit prompt live in `docs/SECURITY.md` /
  `docs/SECURITY-AUDIT-PROMPT.md`. The e2e SHOULD assert the negative case (the
  forbidden/unsigned/cross-tenant caller is rejected), not just the happy path.
- Unit tests: no mocks for databases. Use interfaces + fakes in `pkg/testutil/`
- Integration tests: use testcontainers-go for real Postgres/Redis/NATS
- Never mock Postgres, Redis, NATS, or filesystem
- Tag integration tests with `-tags=integration`
- **Performance / load testing:** the full protocol (reset → seed big-world →
  warm caches → `make loadtest RPS=… VERIFY=1` → read phase metrics + canary,
  plus the hands-off-host rules that make runs comparable) is the
  `/perf-loadtest` skill — `.claude/skills/perf-loadtest/SKILL.md`. Don't
  improvise a load run; a skipped step makes the numbers incomparable.
- **Hot-path iron rule (thrice-proven):** nothing in the per-campaign bid loop
  may do per-call network I/O — hot loops read in-process copies kept warm by
  background bulk refreshers (see `cmd/dsp/refresh.go`).

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

**Source of truth: [`docs/diagrams/README.md`](docs/diagrams/README.md)** — the
diagram index with a per-diagram "Update when" column and a shared visual legend.
Don't duplicate that table here (it drifts); check it there.

**Rule: if your PR changes how services connect, or a data / money / event flow,
it MUST update the matching diagram in the same PR** — find it via the "Update
when" column in `docs/diagrams/README.md`, then `make diagrams` to re-render. CI
can flag `cmd/`/`k8s/` changes that touch no `.d2`.
