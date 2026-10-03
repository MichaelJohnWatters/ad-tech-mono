# Identity Consumer Service

Builds the identity graph off the hot path. Serving pods (SSP ad-tag requests, exchange Prebid inbound, tracker) publish cheap per-request identity observations; this ONE consumer batches, dedupes, and writes the resulting edges to `identity_graph`. Those edges power cross-device/view-through attribution and DSP identity resolution.

## Responsibilities

- Consume `ObservedEvent`s (identity signals: publisher_user_id, uid2, hashed_email, ifa, IP+UA fingerprint) from NATS
- **Deterministic edges** — 2+ identifiers on one request = same person (confidence 1.0)
- **Probabilistic edges** (opt-in, `identity_consumer.probabilistic_enabled`, default off) — different ids seen from the same IP+UA fingerprint linked at `probabilistic_confidence` (default 0.5); fingerprints with > `fingerprint_max_users` ids (default 5) are treated as shared IPs and NOT linked
- Batch + dedupe in-memory (`seen_cap`, default 100k), flush to Postgres every `flush_interval` (default 10s)

## Interfaces

- HTTP :8092 (`routes.PortIdentityConsumer`) — `/healthz`, `/readyz`, `/metrics` only; no business endpoints
- NATS **consumes**: `adtech.identity.observed` (`events.SubjectIdentityObserved`), queue group `constants.NATSGroupIdentityConsumer`
- NATS **publishes**: nothing
- Publishers of the subject: SSP (`cmd/ssp/identity.go`, gated by `SSP_IDENTITY_OBSERVE_ENABLED`), exchange (Prebid inbound), tracker — all via the shared `pkg/identityobserve.Publisher` (nil-safe, best-effort, skips when there's nothing to link)

## Key Packages Used

- `pkg/identityobserve/` - the core: `Observer` (single goroutine owns dedup set + fingerprint buckets, no locking; `Observe()` enqueues, drops if full), `Publisher`, `FPStore` interface
- `pkg/store/postgres/` - `LinkIdentity` edge writes
- `pkg/cache/redis/` - client for the Redis-backed fingerprint buckets (cmd-local `redisfp.go`, keys `identity:fp:{fp}`, TTL `fingerprint_ttl` default 1h)
- `pkg/config/keys/` - `keys.IdentityConsumer.*` (all TierStatic)

## Dependencies

- NATS JetStream (event source — readiness fails without it)
- Postgres (`identity_graph` write target — readiness pings it)
- Redis (fingerprint buckets; `identity_consumer.redis_url`, auto-falls-back to the platform `redis.url` when unset)

## CRITICAL: Replicas & fingerprint state

- Probabilistic linking needs ONE coherent view of the fingerprint buckets. With **in-memory** buckets (no Redis reachable) that means **exactly 1 replica** — more silently splits linking.
- In-cluster the FP store is **Redis-backed and auto-safe** (falls back to the platform Redis when the dedicated key is unset), so helm runs `replicas: 3` (`k8s/helm/adtech/values.yaml`, `IDENTITY_CONSUMER_REDIS_URL=redis:6379`). If Redis is unreachable in-cluster it logs ERROR (broken deps must be loud) and degrades.
- The FP-store read-modify-write is deliberately non-atomic — a race means a few extra links, deduped downstream.

## CRITICAL: Other invariants

- **Boot-retry doctrine (deaf-on-boot class):** a Subscribe that fails once at boot must retry-until-stick (15s loop in `main.go`) — a latched failure once left this consumer DEAF for 5h+ with no edges written. Same doctrine as webhooks/notifications.
- Malformed payloads are poison messages: log + **ack** (never redeliver).
- Everything is best-effort and idempotent: edge re-writes are safe (the dedup set resets when `seen_cap` is exceeded), `Observe` drops on a full channel rather than blocking.

## Architecture Details

See `docs/PLAN.md` -> "Identity and First-Party Data", "Identity Graph", "NATS Subjects"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New NATS subject consumed/published?** Update `docs/PLAN.md` -> NATS Subjects table + NATS Event Flow diagram
- **New publisher of `adtech.identity.observed`?** Update the NATS Event Flow diagram
- **New dependency?** Update `docs/diagrams/architecture.d2` and run `make diagrams`
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
