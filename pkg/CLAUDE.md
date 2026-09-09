# pkg/ - Shared Packages

All reusable libraries live here. Every service imports from `pkg/`. Nothing is duplicated across services. No external shared package registries.

## Package Map

| Package | Purpose | Key interface/pattern |
|---|---|---|
| `models/` | Domain models (Campaign, Creative, Placement, etc.) | Shared structs, used everywhere |
| `auction/` | Auction logic (first-price, strategies) | Deal priority lives in `deals/`; SmartRouter fan-out routing lives in `optimise/` |
| `targeting/` | Targeting rule evaluation engine | Evaluates if a bid request matches campaign rules |
| `pacing/` | Budget pacing calculations | Spread spend evenly over campaign flight |
| `deals/` | Deal types (PMP, PG, preferred), matching, priority | Deal priority order in exchange |
| `openrtb/` | OpenRTB request/response types | JSON serialisation, spec compliance |
| `store/` | Database access layer | Interface per store type, implementations in subdirs |
| `store/postgres/` | PostgreSQL implementation | Multi-tenant: always reads account_id from context |
| `store/analytics/` | ClickHouse implementation (hot) + HotColdStore routing to the Parquet lake (cold) | DuckDB deleted in ADR 0006; cold reads via CH `s3()` |
| `store/objects/` | Filesystem + S3 implementations | Pluggable behind interface |
| `store/datalake/` | Parquet read/write, Delta Log | Used by pipeline and rollups |
| `events/` | Event bus interface | `EventBus` interface - Publish/Subscribe/Ack/Nak |
| `events/natsbus/` | NATS JetStream implementation | Swappable for Kafka later |
| `config/` | Configuration loading | defaults -> env vars -> live Postgres config; every key is a typed handle in `config/keys/` (`keys.DSP.NoisePct.Get(cfg)`) — no magic strings at call sites |
| `middleware/` | HTTP/gRPC middleware | Auth, tenant, rate limit, circuit breaker, audit |
| `logger/` | Structured logging (slog) | JSON output, trace_id in every line |
| `proto/` | Protobuf definitions + generated Go code | `proto/internalrpc/` is the live gRPC contract (internal hot-path twins); the root .proto files are message-model definitions only |
| `grpcx/` | Internal gRPC transport (servers, clients, handler bridge) | gRPC ONLY on edges we own both ends of; `grpc://` vs `http://` URL scheme picks transport; `Bridge` runs the same http.HandlerFunc on both |
| `cache/` | Layered cache (L1 + L2) | In-process + Redis, NATS invalidation |
| `cache/redis/` | Redis client wrapper | Atomic counters, typed get/set |
| `health/` | Health check registration | `/healthz` and `/readyz` endpoints |
| `lifecycle/` | Graceful shutdown | Signal handling, drain, flush |
| `identity/` | Identity graph | Matching, merging, cross-device linking |
| `audience/` | Audience segments | CRM upload, segment matching |
| `privacy/` | Consent and privacy | Consent checking, opt-out, deletion propagation |
| `fraud/` | Fraud detection | Real-time checks, scoring, blocklists, ads.txt |
| `billing/` | Billing logic | Spend calculation, invoices, reconciliation |
| `reporting/` | Report builder | Query construction, pre-built templates |
| `audit/` | Audit logging | Log() function, actor context extraction |
| `email/` | Email sending | Interface (Mailpit / SES), template rendering |
| `pipeline/` | Data pipeline logic | Format detection, validation, normalisation, enrichment |
| `testutil/` | Test helpers | Fixtures, fakes, e2e harness, tenant isolation tests |

## Conventions

- **Interfaces at package root, implementations in subdirectories** (e.g. `events/events.go` has the interface, `events/natsbus/` has the implementation)
- **Multi-tenancy in every store method** - see root CLAUDE.md
- **No direct NATS/Redis/Postgres imports in services** - always go through `pkg/` abstractions
- **Proto changes require `make proto`** - regenerates Go code via Buf
