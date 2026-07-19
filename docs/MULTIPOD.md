# Multi-Pod Support

Which services can run with multiple replicas, which cannot, and — the part
that matters — **why**. The dividing line is always the same question:
*where does this service keep state, and is that state shared or per-process?*

Verified by the 2026-07-19 multi-pod load run: 10 services at 2 replicas
(tracker at 3) under 30 minutes of steady load (135,517 requests), with
rolling restarts of ssp and dsp-internal **mid-load** — both restarts were
client-invisible. The run's one error burst (3,487 requests over ~90s) was
NOT multi-pod related: postgres's liveness probe (default 1s exec timeout)
failed under sustained load and kubelet gracefully killed the database —
probe budgets now sized so a busy-but-healthy postgres can't be probe-killed
(see templates/infra/postgres.yaml). Exactly-once held: 213,368 impressions,
213,368 unique trace_ids, zero duplicates.

## Scales freely

State lives in shared infra (Postgres / Redis / NATS / object storage) or is
request-scoped; replicas are interchangeable.

| Service | Why it's safe |
|---|---|
| `gateway` | Stateless HTTP: sessions are JWT cookies (verified per request, no server session store); warm caches are per-pod read-only replicas fed by polling + NATS invalidates (per-POD consumer groups, so every replica hears every invalidate). |
| `ssp` | Placement/quality warm caches are read-only; household derivation is a pure function (hh:HMAC(salt,IP)); audience data reads through Redis/PG. |
| `exchange` | Auctions are request-scoped. The smart router learns in-memory **per replica** — replicas diverge slightly and converge independently (warm-started from analytics at boot). Acceptable by design: routing is an optimisation, not correctness. |
| `dsp-*` | Campaign warm cache read-only; budgets, pacing commitments, balance gate, and frequency caps are all atomic Redis operations (L2 is THE shared-mutable-state tier precisely so bidding replicas can't disagree). |
| `adserver` | Creative cache read-only; frequency caps in Redis (shared across replicas — co-viewing counts survive whichever replica serves). The creative bandit learns in-memory per replica (same divergence argument as the exchange router). |
| `tracker` | Stateless beacon validation (HMAC); event dedup keys in Redis; publishes to NATS with `Nats-Msg-Id` so even a republish from a dying replica can't double-enter the stream. Runs 3 — single-replica tracker readiness-flaps under load were the source of the old ~1% impression slippage. |
| `publisher-adserver`, `ssai` | Request-scoped serving. SSAI session continuity across replicas hasn't been exercised at depth (the load runs stitch fine at 2 replicas); if real SSAI sessions ever misbehave, session affinity on the Service is the tool. |
| `webhooks` | NATS durable queue group → each event is delivered to exactly ONE replica; delivery log and retry state in Postgres. |

## Scales **with a required config**

| Service | Requirement | Why |
|---|---|---|
| `reporting` | `reporting.shared_pacing_counter=true` (set in local values) | Two multi-replica mechanisms: (1) the shared Redis additive committed-spend counter — each replica consumes a PARTITION of the event stream (durable group), so in-memory accumulators would each see partial spend; (2) the pacing SNAPSHOT PUBLISHER is single-writer by nature, so replicas run a Redis-lease election (SetNX, TTL = 3 ticks) and only the leader publishes — the 2026-07-19 run caught both replicas publishing conflicting snapshots before the election existed (the old guard could only ERROR about it). Everything else is already safe: inserts are exactly-once, billing settles idempotent by trace, rollups idempotent. Known best-effort edge: balance drawdowns during a Postgres outage are logged + lost (~\$3 of ~\$912 during the probe-kill window), never double-applied. |
| `identity-consumer` | `identity_consumer.redis_url` set (local values: `redis:6379`) | Probabilistic linking buckets fingerprints by IP+UA over a time window. In-memory buckets on 2 replicas would split the fingerprint space — two events that should link might land in different pods and never meet. Redis-backed buckets keep the window coherent across replicas. |

## Must stay single-replica

| Service | Why it CANNOT scale (today) |
|---|---|
| `pipeline` | **The lake's single writer.** Delta commits are serialized by an in-process mutex, and version allocation is list-the-log-then-max+1 — there is no cross-process commit protocol. Two writers would race version numbers; the `putLogFile` overwrite guard turns that race into a crash instead of corruption, but either way a second replica cannot safely commit. Scaling this requires real optimistic-concurrency Delta commits (conditional puts / If-None-Match) — a deliberate non-goal for now. Compaction/purge/vacuum run *inside* this process for the same reason. |
| `report-runner` | The job QUEUE is multi-worker safe (`FOR UPDATE SKIP LOCKED`) — the blocker is the **boot-time stuck-job requeue**, which assumes "any `running` row is mine from a previous life." A second replica booting would requeue jobs the first is actively executing → duplicate artifacts/emails. Fixable with a lease/heartbeat per job; until then, 1. |
| `batch-conductor`, `dayboundary` | CronJobs, inherently one run at a time. The chain's steps assume exclusive ownership of their windows (checkpoint→compact→rollups→…), and compact/vacuum call into the pipeline's single-writer lock anyway. |
| `transcoder` | No known blocker, but unverified under concurrency — kept at 1 with no claim either way. |

## Infra (local dev is single-instance by design)

postgres, nats, redis, clickhouse, minio, tigerbeetle each run 1 locally.
The prod story is different machinery, not more local pods: NATS JetStream
replicas via `NATS_STREAM_REPLICAS=3`, Postgres replication, Redis
cluster/sentinel, ClickHouse replication, TigerBeetle clusters. Scaling any
of these locally buys nothing and complicates recovery.

## Known caveats at >1 replica

- **`POD_NAME` is a deployment identity, not a pod identity.** The chart
  hardcodes it (`dsp-internal-0`, `tracker-0`, …), so per-pod config rows
  (`config.pod_id`) apply to ALL replicas of that deployment. "Per-pod"
  config granularity is effectively per-deployment under multi-pod. True
  per-pod control would need downward-API `POD_NAME` plus a story for config
  rows keyed to ephemeral pod names — deferred until someone actually needs
  replica-level divergence.
- **In-memory learners diverge per replica** (exchange smart router,
  adserver bandit). Each replica explores/converges on its own, warm-started
  from shared analytics. This is accepted: they're optimisers, and their
  input signal is shared even though their state is not.
- **`/debug/cache/refresh` is load-balanced — FIXED by broadcast.** A
  refresh call reaches one replica; that replica now also publishes the
  cache's NATS invalidate, so every OTHER replica reloads within seconds
  (per-POD consumer groups; verified live: refresh on pod A → pod B logs
  "warm cache invalidate received, reloading"). Before the fix, one
  adserver replica served ~340 default-HTML fallbacks from a stale cache
  while its freshly-refreshed sibling was fine.
- **Graceful shutdown order matters more.** A draining pod that closes its
  NATS bus before the HTTP server finishes draining loses whatever its last
  in-flight requests try to publish (observed: 3 behaviour events on an ssp
  drain). Fixed at the lifecycle layer: `lifecycle.ServeHTTP` registers the
  HTTP drain via `OnShutdownFirst`, so it always precedes dependency
  teardown, in every service.
- **Warm-cache staleness windows are per-replica.** A failed LoadAll on one
  replica leaves THAT replica stale until its next successful poll; the
  other replicas are unaffected. Under multi-pod this becomes partial
  degradation (some requests see stale data) instead of total — better,
  but harder to notice. The retrying loaders + polls self-heal.
- **e2e harness**: the suite's reporting `/debug` read-backs go through the
  load-balanced Service; with 2 reporting replicas a read-back may hit the
  replica that didn't ingest the event. The preflight pins reporting where
  it matters; if e2e flakes appear at 2 replicas, scale reporting to 1 for
  the suite run.

## Current local replica map

Set in `k8s/helm/adtech/values.yaml`: tracker 3; gateway, ssp, exchange,
dsp-internal, adserver, reporting, webhooks, identity-consumer,
publisher-adserver, ssai at 2; everything in the "must stay single" table
at 1 (annotated in values — do not bump those without reading this doc).
