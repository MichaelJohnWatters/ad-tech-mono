# Audience & Auction Data Path — Scaling Notes and Future Work

Scope: how audience data flows from a pixel/upload into a bid, why the current
design is shaped the way it is, what has already been optimised, and the
concrete future-work steps (near-term) plus the hyperscale swaps (long-term).

This is a **notes / future-work** doc, not an ADR — nothing here is committed to.
It captures the reasoning from the audience-preloader delta work so the next
engineer doesn't have to re-derive it.

---

## 1. The rule that decides where data lives

The single principle behind the whole data path:

- **Keyed by campaign → bounded → in-process pod RAM** (`pkg/cache/warm`, Go maps).
  Campaigns, placements, creatives, deals, budgets, opt-outs, fraud rules, ads.txt.
  There are thousands of these; they fit in every pod easily; bid-time lookup is a
  nanosecond map read.
- **Keyed by user → unbounded by user count → shared Redis** (`pkg/audience/store/preload`).
  `audience_segment_members` (user → segments). Millions–hundreds of millions of
  users in production; replicating that into every pod's RAM would blow up memory,
  so it lives once in Redis. Bid-time lookup is one Redis GET (~0.2ms).

`dsp_private` (retargeting/private) membership follows the *same* rule as public —
it's user-keyed and unbounded (an advertiser's 30-day retargeting pool is millions
of users), so it is in Redis too, **not** pod RAM. Visibility does not change the
partitioning; "campaign-keyed vs user-keyed" does.

Store roles:
- **Postgres** = mutable source of truth for membership (upsert/delete/TTL — things
  ClickHouse is bad at).
- **ClickHouse** = append-only signals (`behaviour_signals`, `profile_signals`),
  read in aggregate by the batch profile-builder.
- **Redis** = the hot read cache the bid path actually reads.

## 2. The auction read path (current)

Audience membership is a property of the **user/request**, not of each campaign, so
it is resolved **once per request** and then every candidate campaign's targeting
tests against that one set.

```
SSP builds bid request
  └─ 1 Redis lookup: user's PUBLIC segments → stamp user.data (for external bidders)
EXCHANGE fans out to DSPs
  └─ OUR DSP, per request:
       a) 1 Redis lookup: user's DSP-PRIVATE segments (+ public already on request)
          — cmd/dsp/identity.go DSPSegmentsForUser, 25ms deadline, degrade to no-enrich
       b) candidate campaigns are already in pod RAM (warm cache)
       c) targeting.Evaluate each campaign against {request + resolved segments}
       d) price + bid modifiers (incl. audience modifiers) for the eligible ones
       e) return best bid
EXCHANGE runs the auction (first-price + shading) → winner → adserver → tracker
```

External/competitor DSPs do no lookup — they read the public segments off the bid
request we send them.

## 3. Two-speed audience updates

| Path | Latency | Mechanism |
|---|---|---|
| Real-time retargeting (single-visit rules, `min_count ≤ 1`) | seconds | pixel → `audience-rt` enroll → membership → preloader delta → Redis |
| `behaviour_signals` row in ClickHouse | seconds | reporting consumer block-insert |
| Batch segments (`min_count > 1`, behavioural, lookalike, composite) | up to ~1h | batch-conductor CronJob `10 * * * *` → profile-builder |

`min_count ≤ 1` can fire instantly (a single visit is self-contained). Anything
needing history/aggregation is a ClickHouse `GROUP BY` → the hourly profile-builder.

## 3b. IN PROGRESS — append-based change-log cache (the end-state)

Being built as a safe parallel migration (write new path → verify → flip read → delete old):
- **Stage 1 (done):** L2 set-ops, `audience_membership_changelog` table + store methods.
- **Stage 2 (done):** a DB trigger on `audience_segment_members` appends every write
  (enroll/suppress/upload/profile-builder/prune/TTL-purge) to the change-log — the
  transactional outbox, no per-writer code. A SINGLE writer in `pipeline` drains it,
  applying atomic SADD/SREM to a parallel Redis-SET namespace (`audience:set:…`) +
  a full-scan reconcile. Verified: the SET keys match the live `audience:user` JSON.
- **Stage 3 (deferred — read NOT flipped):** the DSP/SSP read still uses the JSON
  path. Flip `preload.lookup` to `SMEMBERS(audience:set)` only after hardening, because
  a flip surfaced two bid-hot-path reliability gaps:
  1. **Reconcile is non-atomic** — `Delete(key)` then `SAdd(key, …)` leaves a
     transient-empty window; a bid landing in it mis-targets. Fix: build into a temp
     key + atomic `RENAME`, or `SADD` current + `SREM` only the stale diff (no delete).
  2. **Writer Redis reliability** — the pipeline `SelfHealingL2` was observed serving
     from the in-memory fallback after a transient dial issue, silently dropping
     writes (reconcile logged "keys:N" while Redis had 0). Needs a readiness gate /
     hard-fail-if-not-on-Redis for the single writer, since nothing else writes it.
  The append/trigger/single-writer machinery is all in place and running in parallel;
  only the read-flip + these two fixes remain.

## 4. DONE — delta preloader (commit b8d00ae)

The DSP/SSP preloader used to full-scan `audience_segment_members` + rewrite every
Redis key on a 30s tick *and* on every `cache.invalidate.audience`. Now:

- `events.AudienceInvalidateEvent{UserIDs|SegmentID|...}` names what changed.
- Preloader re-materializes only those users (`refreshUsers`) or that segment's
  users (`refreshSegment`); id-less/garbled → full reconcile.
- The periodic full scan is now a **reconciler** (`audience.preload_interval` 30s→5m,
  `cache_ttl` 90s→15m; `New()` enforces `ttl ≥ 2×interval`).

See `pkg/audience/store/preload/preload.go` and memory `project_audience_preloader_delta`.

## 5. NEAR-TERM future work (in priority order)

### 5.1 Write-through for the real-time retargeting path
**Problem:** `cache.invalidate.audience` is a **broadcast to three consumers** —
DSP preloader, SSP preloader, and the SSP taxonomy cache (`cmd/ssp/taxonomy.go`).
A retargeting enrollment is `dsp_private`, so:
- DSP preloader → relevant.
- SSP preloader → wasted (SSP only reads public keys).
- SSP taxonomy cache → wasted **and** a full map rebuild (it ignores the payload —
  `c.refresh(ctx)` on every event).

So the highest-frequency path wakes two consumers that can't be affected by it, one
of them doing a full rebuild. And the DSP re-reads Postgres for the row we just wrote,
once per pod (read-back fan-out that scales with enrollments × pods).

**Fix:** `audience-rt` writes the user's `dsp_private` Redis key **directly**
(write-through) on enroll/suppress and publishes **nothing** for that path. The DSP
reads Redis unchanged; the reconciler stays as drift insurance.
- Preferred value type: **Redis SET** (`SADD`/`SREM`) — atomic, no read-modify-write
  race across `audience-rt`'s replicas. Trade-off: changes the stored value from a
  JSON array → set, so it touches the DSP read + preloader write.
- Cheaper diff: JSON GET+merge+SET (keeps format) but has a small cross-replica race
  corrected by the reconciler.
- Safe because retargeting is `dsp_private` and only the DSP reads `dsp_private`.
  Verify that invariant in code; optionally fall back to a broadcast if a retargeting
  segment is ever public (not the case today).

### 5.2 Visibility-aware invalidates (fix the taxonomy-cache waste)
The bulk path (uploads, profile-builder) still broadcasts — correctly, because it can
create **public** segments the SSP + taxonomy need. But it also broadcasts for
`dsp_private` changes, and the taxonomy cache full-rebuilds on every one.

**Fix:** add `Visibility` to `AudienceInvalidateEvent`; the SSP taxonomy cache skips
`dsp_private` events (taxonomy is public-only) instead of full-rebuilding. Bonus:
make it refresh only the changed segment rather than the whole map.

### 5.3 Tighten TTL-expiry propagation (optional)
A retargeting member whose `expires_at` passes emits no event, so it clears from
Redis only at the next reconcile (≤5m) — acceptable (suppress-on-purchase is
immediate; abandoner-ages-out is low-stakes). To tighten: have
`PurgeExpiredMembers` emit user-targeted invalidates for the users it purges.

### 5.4 Bounded L1 in front of Redis (only if latency profiling asks for it)
A small in-process LRU of recently-seen users' segment lists in the DSP: hot/repeat
users hit RAM (nanoseconds), everyone else falls through to Redis. RAM stays capped
by LRU size, not total user count. This is a **latency** optimization, not a
correctness one — at ~0.2ms per GET against a 10–100ms bid budget it usually isn't
worth the extra invalidation surface (the L1 would need to honor the same delta /
write-through updates). Add only if the Redis GET shows up in bid-path profiling.

## 6. LONG-TERM — hyperscale swaps

These are pure **scale** swaps, not design corrections. The current patterns are
industry-standard; at millions-of-QPS / billions-of-users these are the frontiers.

| Today | Hyperscale | Why / trigger |
|---|---|---|
| **Redis** for the user/audience store | **Aerospike** (RAM+SSD KV, the de-facto ad-tech user store) | Redis is RAM-bound + single-threaded per shard. Trigger: Redis memory/throughput becomes the cost/latency ceiling. |
| **Postgres** as membership source of truth | write straight to the KV store (no relational truth for user data) | A relational truth for user data caps write rate — the preloader-scan / write-heavy-retargeting pressure is this limit. Trigger: Postgres write throughput on membership saturates. |
| **Shared Redis** read by all DSP pods | **shard the bidder by user** (route each request to the pod holding that user's slice in RAM) | Removes the network hop; makes the bidder stateful and requires the exchange to route by user id. Trigger: the Redis hop dominates bid latency even with an L1. |

## 7. When to reach for each (signal → action)

- Invalidate storm / preloader CPU high → **5.1 write-through** (removes the broadcast).
- SSP taxonomy CPU high on `dsp_private` churn → **5.2 visibility-aware invalidates**.
- Bid-path p99 dominated by the audience Redis GET → **5.4 L1 LRU**, then **§6 Aerospike / sharding**.
- Postgres membership write throughput saturating → **§6 KV-as-truth**.
- Redis memory cost / cluster-shard limits → **§6 Aerospike**.

## 8. References

- Preloader: `pkg/audience/store/preload/preload.go`
- Invalidate event: `pkg/events/payloads.go` (`AudienceInvalidateEvent`)
- Publishers: `cmd/audience-rt/main.go`, `pkg/ingest/processor.go`,
  `pkg/profilebuilder/builder.go`, `cmd/gateway/taxonomy.go`
- Three subscribers: DSP `cmd/dsp/main.go:198`, SSP `cmd/ssp/main.go:138`,
  SSP taxonomy `cmd/ssp/taxonomy.go`
- Warm (in-process) caches: `pkg/cache/warm/warm.go`
- Bid-time consumption: `cmd/dsp/identity.go` (`DSPSegmentsForUser`),
  `pkg/targeting` (targeting + modifiers)
- Batch chain: `pkg/batch`, cron `k8s/helm/adtech/values.yaml` (`batch-conductor`)
