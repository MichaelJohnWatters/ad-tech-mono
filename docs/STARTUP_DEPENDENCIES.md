# Service Startup Dependencies

> Why services wait on each other at boot, what's load-bearing about that,
> and the levers available if we ever want to relax it.

## The wait chain

Two layers gate when a service is considered "ready" to receive traffic:

### Layer 1: Tilt `resource_deps`

Each service declares hard infrastructure dependencies in the `Tiltfile`:

| Service | Waits for |
|---|---|
| gateway, dsp×3 | postgres + redis |
| exchange, reporting | postgres + nats |
| tracker | nats + redis |
| ssp | postgres |
| adserver | minio + redis + postgres |
| publisher-adserver | postgres + **ssp + adserver** ← cascades |

Until those dependencies report Ready (pod-level via `kubectl wait`), Tilt
won't run the service's `serve_cmd`. Publisher-adserver is the worst
offender: it waits not just for infra but for two other services to be up.

### Layer 2: `/readyz` first-load gates

Most services register readiness checks for their warm caches:

```go
hlth.AddReadinessCheck("secrets-cache", func(_ context.Context) error {
    return secretsCache.Ready()  // errors until first successful load
})
```

So even after the process starts, Tilt won't mark it "ready" — and
anything depending on it stays blocked — until the cache fetches at
least once. If Postgres comes back slowly, those first loads fail
repeatedly and the gate stays red.

## Why this exists

**Fail-fast diagnostics.** Operators want to know immediately if a
dependency is misconfigured rather than have services pretend to be
healthy with empty caches. The wait chain is correct for prod: a
service exposing `/readyz=200` with no campaigns loaded would serve
"no-bid" to every request, looking like a tenant issue when it's
really a startup race.

## Why it stings in dev

After a Colima blip the cascade dominates the recovery time:

```
postgres pod restarts          (~60s)
→ all services with postgres in resource_deps wait
→ publisher-adserver also waits for ssp + adserver
→ ssp + adserver wait for postgres
→ /readyz gates on first cache load
→ full stack functional ≈ 90s after Colima returns
```

The self-healing warm-cache loaders (shipped 2026-06-03 in
`pkg/cache/warm/retrying.go`) make the cache side of this safer —
loaders re-pick the Postgres connection on every poll until it
succeeds, so a missed initial load no longer pins the cache to empty
for the process lifetime. But Tilt + /readyz haven't caught up.

## Levers, if we want to relax

Three independent changes, each ships separately:

**1. Drop infra `resource_deps`.** Remove postgres/redis/nats/minio from
each service's `resource_deps=[…]`. Services boot in parallel.
Self-healing loaders absorb the gap; readiness probes still gate
traffic via /readyz. Safest of the three.

**2. Drop the secrets-cache /readyz gate.** Change
`secretsCache.Ready()` to always return nil. Service marks itself
ready with an empty cache; auth requests get 401 until the cache
fills (≤30s, usually <2s). Acceptable in dev; in prod you'd want this
gated behind a config flag.

**3. Drop publisher-adserver's deps on ssp+adserver.** Publisher-adserver
only calls them at request time, not boot time. The Tiltfile dep is
over-conservative. Cuts the worst cascade.

The three together = "spin up and poll" model. Estimated effort
~15 min including a fresh-Colima verification. Estimated recovery
time after a Colima blip: ~90s → ~30s (one warm-cache poll interval).

## Decision (2026-06-03)

Held off — the current behaviour is correct for fail-fast. Revisit
when the dev cost of the cascade crosses a threshold (e.g. Colima
blips become more frequent or a new service piles more onto the
wait chain). The levers above are reversible and independent, so
pick them up one at a time when needed.
