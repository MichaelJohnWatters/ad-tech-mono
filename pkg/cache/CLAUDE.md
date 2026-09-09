# pkg/cache - Layered Cache

The caching layer for the hot path: L1 in-process (per-pod), L2 Redis (shared), L3 Postgres as source of truth (via `pkg/store/postgres`). Invalidation rides NATS `adtech.cache.invalidate.*` subjects (constants in `pkg/events/subjects.go`).

## Key Entry Points

- `cache.New(l2, clk, log)` (`cache.go`) — `Cache{L1, L2}`. `L1Cache` = `sync.Map` + TTL, `Clear`/`ClearPrefix` for invalidation.
- `L2Cache` interface (`l2.go`) — Get/Set/Delete/SetNX/Incr/IncrBy/DecrBy/Expire + set ops (SAdd/SRem/SMembers/`ReplaceSet`). Optional capabilities: `BulkGetter` (use the `MGet` helper, degrades to serial Gets — off-hot-path only) and `Scripter` (`Eval`, server-side Lua — callers MUST fall back to the plain multi-op path on error).
- `redis.Client` (`redis/client.go`) — the real L2 (plus SCard/SIsMember extras). `MemoryL2` (`l2.go`) — the in-memory fake for tests (mutex-guarded, concurrency-safe).
- `NewSelfHealingL2(dial, retryEvery, name, log)` (`selfheal.go`) — the ONLY way services should construct their Redis L2. Fail-open: serves `MemoryL2` while a background loop retries the dial, swaps to real Redis when it lands.
- `NewDedupAdapter(l2)` (`dedup.go`) — bridges L2 to `events.DedupStore` (SetNX) for idempotent consumers.
- `warm.New(warm.Config[T])` + `Start` (`warm/warm.go`) — the generic warm-cache primitive: full entity set in process, lock-free `atomic.Pointer` snapshots, refreshed by sync boot load + poll ticker + NATS invalidate. `Loader[T]` implementations live in `pkg/store/postgres`; optional `SingleLoader[T]` makes a targeted invalidate O(1 row) instead of a full reload.
- `warm.RetryingLoader[T]` (`warm/retrying.go`) — wraps a loader constructor so a Postgres-down boot self-heals on the next poll instead of latching empty (construct/LoadAll failure returns `(nil, nil)` — previous snapshot kept, retry next tick).
- `warm.RefreshHandler(caches...)` (`warm/debug.go`) — the `/debug/cache/refresh` endpoint; on success calls `PublishInvalidate` so ALL replicas refresh, not just the pod the load balancer picked.

## Invariants & Gotchas

- **Boot-latch doctrine**: never latch a fallback from one failed boot attempt. `SelfHealingL2` (Redis dial), `RetryingLoader` (Postgres loader), and the warm cache's `resubscribeLoop` (failed NATS invalidate subscription → poll-only until re-subscribe sticks) all exist because the latch-forever version of each caused real outages.
- `SelfHealingL2` fallback-era counters are DISCARDED on swap to real Redis — same loss as the old pod-bounce cure, just automatic.
- Warm-cache invalidate consumer group MUST be per-replica via `podid.Replica()`, NOT `POD_NAME` (POD_NAME is a stable SHARED value per service — replicas would load-balance invalidates and go stale). And it MUST be `events.SubscribeBroadcast` (ephemeral): per-pod durables leaked one consumer per pod ever created and wedged JetStream (2026-07-25).
- NATS delivery is at-least-once (see `pkg/events/CLAUDE.md`) — invalidate handlers must tolerate redelivery. A full-reload `Trigger` is naturally idempotent; keep it that way.
- `warm.Cache.All()` returns the shared snapshot slice — iterate, NEVER modify.
- Targeted (single-id) invalidate is skipped when `Config.OnRefresh` is set (the hook needs the full set) or `LoadOne` errors — safe default is full reload. Log the LoadOne error; swallowing it hid a mis-scoped loader for hours (2026-07-26).
- `ReplaceSet` is atomic (MULTI/EXEC DEL+SADD+EXPIRE) so hot-path `SMEMBERS` never sees an empty/half-built set. Use it for rebuilds; SAdd/SRem for appends (see the audience cache).
- `redis.Client` sets `ContextTimeoutEnabled` so ctx deadlines bind ON THE WIRE — without it the 25ms hot-path caps provably didn't bind (2026-08-05). Don't construct go-redis clients elsewhere.
- Hot-path iron rule: the per-campaign bid loop reads warm in-process snapshots only — no per-call network I/O (see `cmd/dsp/refresh.go` and root CLAUDE.md).
- Redis keys are prefixed by service name (`dsp:budget:{id}`, `adserver:freqcap:{id}`).
- Warm poll cadence is per-entity config: `cache.warm.{entity}.poll_interval` keys in `pkg/config/keys/` (typically 30s; fraud_rules/signing_keys 60s, ads_txt/billing_rates 300s). Redis pool size via `redis.pool_size`.

## Used By

Every serving service: DSP/SSP (via shared setup)/adserver/publisher-adserver/tracker/gateway/reporting/pipeline construct `SelfHealingL2`; DSP/SSP/exchange/adserver/publisher-adserver/tracker/gateway/reporting run warm caches. `pkg/secrets` wraps `warm.Cache[Secret]` for its secrets cache.

## Testing

- `MemoryL2` for unit tests — no Redis, per project rule (no mocked infra; fakes only).
- Warm caches take an injected `clock.Clock`; `Config.Bus = nil` gives poll-only mode for tests.

## Architecture Details

See `docs/PLAN.md` -> "Layered Cache Architecture", "Cache Invalidation", "Cache Invalidation Subjects" (Core NATS, fire-and-forget), "Per-pod warm-cache consumer collision fix".

## Diagram Updates

- **New `adtech.cache.invalidate.*` subject?** Add the constant to `pkg/events/subjects.go` AND update `docs/PLAN.md` -> NATS Subjects table.
- **Changed how invalidation propagates (broadcast/group semantics)?** Update `docs/PLAN.md` -> Cache Invalidation + the NATS Event Flow diagram, then `make diagrams`.
