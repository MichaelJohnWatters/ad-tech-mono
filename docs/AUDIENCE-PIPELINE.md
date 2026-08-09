# The Audience Pipeline, End to End

*Written from a live walkthrough on 2026-08-09 (handoff 07, Phase 1). Every
row, Redis key, and timestamp below was observed on the running stack, not
inferred from code. Companion diagram: `docs/diagrams/targeting-data-flow.d2`
(updated the same day to match reality).*

## The mental model in one paragraph

Audience membership is a Postgres table — `audience_segment_members
(segment_id, user_id, account_id, expires_at, source, origin_trace)` — with
**three ways in and one way out**. In: (A) file uploads through one ingest
queue, (B) behaviour rules recomputed hourly by the profile-builder, (C) a
real-time enroller for single-visit retargeting. Out: a DB trigger appends
every write to a changelog outbox; **one** drainer in `cmd/pipeline` applies
it to Redis SETs; the DSP/SSP read those sets at bid time and never write
them. Everything else — TTL aging, GDPR purge, composite/lookalike
derivation, segtax stamping — is a refinement of one of those four arrows.

```
uploads ──► audience_ingest_jobs ──► pkg/ingest.Processor ──┐
behaviour ► ClickHouse ► hourly profile-builder ────────────┼──► audience_segment_members
pixel ────► NATS ► audience-rt (seconds, min_count<=1) ─────┘         │ trigger (mig 078)
                                                                      ▼
                                            audience_membership_changelog (outbox)
                                                                      │ ONE drainer, cmd/pipeline
                                                                      ▼  3s delta / 5m reconcile
                                                    Redis audience:set:{user}:{visibility}
                                                                      │ read-only SMEMBERS
                                                                      ▼
                                             SSP (public stamp, segtax) · DSP (dsp_private union)
```

## The themed world (how to check any of this by hand)

The local world is deliberately readable (design decision 2026-08-07,
two-tier): a **readable core** of themed segments whose membership is
*earned*, and an optional **synthetic tier** for density testing
(`cmd/seed --synthetic-segments N`) that never pollutes the story.

- Themed publishers (`profiles/publishers/themed.yaml`): Waggy Tails Daily
  (`dogs`), Purrfect Living (`cats`), Third Wave Times (`coffee`), Sweat
  Zine (`fitness`). Placement categories are those plain slugs.
- Themed segments (seeded, `cmd/seed/extras.go seedThemedSegments`): Dog
  Lovers / Cat Lovers / Coffee Snobs / Fitness Fans are *behavioural rules*
  (`{"event":"request","category":"dogs","min_count":3,"window_days":30}`),
  plus one composite (Dog Cafe Crowd = dog-lovers ∧ coffee-browsers), one
  lookalike (seeded from Dog Lovers), and one min_count-1 retargeting
  segment (Dog Food Cart Abandoners, tag `dogfood-cart`).
- Themed personas (`pkg/simulator/request.ThemedPersonas`, run with
  `go run ./cmd/simulator run --profile themed`): dog/cat/coffee/fitness
  people who browse their topic 80% of the time under **stable pooled user
  ids** (`dog-lover-mobile-u007`), so repeat visits accumulate. The default
  personas keep their per-request random ids — that's why they can never
  earn frequency-rule membership, and why perf profiles are unchanged.
- Themed campaigns (`profiles/dsps/internal.yaml`): "Premium Dog Food Co -
  Dog Lovers" targets `seg-dog-lovers`, etc. Bids are premium-tier (12-14)
  so whenever the audience matches, the themed ad wins the auction.

Verification therefore reads like English, and all of it was observed live:
after 2 minutes of themed traffic and one conductor run, **the dog-food ad
won for `dog-lover-desktop-u009` on the dog blog (first attempt), and did
not appear in 10/10 serves for a cat person on the same page**. The GPC cat
persona browsed cat pages for the whole run and left **zero** behaviour rows.

---

## Path A — uploads (the slow path for files)

One queue, one processor, two producers. Everything lands in
`audience_ingest_jobs` (migration 054) and runs through
`pkg/ingest.Processor` — whether it came from the portal/API
(`POST /v1/api/audiences`, gateway) or from the S3 drop-zone
(`adtech-onboarding/{provider}/incoming/` + `manifest.json`, polled by
`cmd/pipeline`). Small-and-due-now jobs run **inline** in the gateway
request (200 + match rate); big or scheduled (`run_at`) jobs return 202 and
the pipeline worker drains them (SKIP LOCKED + 2-minute leases, crash-safe).

The processor pipeline: PGP decrypt (platform key; `encryption_expected`
providers get cleartext rejected) → decode (magic-byte format sniffing) →
tenant field mappings → **strict all-or-nothing validation**
(`pipeline.ingest_max_reject_pct`, default 0) → `UpsertSegment` +
`AddMembers` → publish `adtech.profile.signal` (chunked, 1000 ids/message)
→ move file to `processed/`/`rejected/` → completion email.

Observed live (3-row CSV, "Dog Owners CRM List", inline):

```
audience_ingest_jobs: done | api | total 3 valid 3 | file_key api/d824.../Dog-Owners-CRM-List.csv
                      job id 55607265-a7f3-4096-...
audience_segment_members: crm-dog-owner-001 | source=api | origin_trace=ing_55607265a7f340968b672a36f06522ce
ClickHouse profile_signals: user_id | crm-dog-owner-001 | api | ing_55607265...
Redis (≤3s later): audience:set:crm-dog-owner-001:dsp_private → {c613498f-...}
```

The lineage rule: the job UUID becomes `ing_<32hex>`, stamped on member rows
(`origin_trace`) *and* every profile_signal — one grep connects a member to
the exact file, job, and row batch that created it.

## Path B — behaviour rules (the slow path for observed conduct)

**Capture.** The SSP publishes one `adtech.behaviour.observed` row per
*consented* ad request (`cmd/ssp/behaviour.go`): the consent gate
(`privacy.Evaluate` over the request's own signals) applies at **capture**,
because a behaviour row is retained data, not a transient annotation. The
placement's content categories are stamped **at event time** from the warm
cache — a rule evaluated months later must not depend on what the placement
claims *then*. The tracker publishes the same event kind for `/v1/t/rt`
retargeting-pixel hits (`site_visit`, with advertiser `account_id` + `tag`).
Reporting consumes the subject and batch-inserts ClickHouse
`behaviour_signals`.

Observed: 120s of themed traffic → 127 `dogs` signals across 62 users; the
GPC persona contributed 0 (consent gate held).

**Recompute.** The hourly batch conductor (CronJob :10, completion-ordered
chain: checkpoint → rollups → parquet-export → **profile-builder** →
privacy) runs `pkg/profilebuilder`: ① union-find identity clustering over
`identity_graph` → `identity_clusters` (household `hh:` ids excluded;
mega-cluster guard), ② behavioural rules as ClickHouse GROUP BYs
(`qualifyingUsersSQL`: count matching rows per user key in the window,
HAVING ≥ min_count), person-level enroll → expand to every id in the
person's cluster → replace-by-segment prune, ③ derived rules (composite set
logic, lookalike category-similarity) in a second pass so they see this
run's behavioural output, ④ reconcile of onboarded profile_signals.

Observed: the 11:15 run enrolled 57 members across the 8 themed rule
segments — `Dog Lovers count=13` with exactly the dog personas' pooled ids
(plus two heavy UID2 "everything-browser" default personas who legitimately
qualified for several interests).

**The bug this walkthrough found.** In-cluster, the conductor connects as
the NOBYPASSRLS `adtech_app` role, and the builder's cross-tenant reads of
`audience_segments`/`members` were unscoped — RLS silently returned **zero
rows**, so every hourly run since the DB-role flip reported
`done enrolled=0` and did nothing, with no error anywhere. Fixed 2026-08-09
(`platformReadTx` around the four reads, the standard hatch pattern);
verified `rule_segments 0→8, enrolled 0→57`. Two durable lessons: (1) any
new cross-tenant reader MUST use the platform hatch — an unscoped query
doesn't fail, it lies; (2) "step done" + a zero-work detail line is an
observability smell the judgement pass should address.

## Path C — real-time retargeting (the fast path)

`cmd/audience-rt` (1 replica, queue-group safe) consumes
`adtech.behaviour.observed` (site_visit) and `adtech.events.conversion`. A
visit that matches an active retargeting rule enrolls the user immediately
(`AddMembersWithExpiry`: upsert with `expires_at = now + window_days`;
`xmax=0` idiom makes enroll-events fire only on FIRST enrollment; repeat
visits just refresh the TTL). A `purchase` conversion removes the user from
all the advertiser's retargeting segments — stop chasing buyers.

**Why only `min_count<=1` rules run here:** a frequency rule ("3+ visits in
30 days") needs *history* — a consumer holding one event can't know the
count without re-querying ClickHouse per event, which would turn a
fire-and-forget consumer into an analytics client on the hot event stream.
One-visit rules need no history, so enrollment is a pure function of the
single event. Frequency rules stay with the hourly batch; that boundary is
the latency/complexity trade, chosen deliberately.

Observed: pixel fired 11:03:53 →
`walkthrough-dog-abandoner-001 | Dog Food Cart Abandoners | source=retargeting |
origin_trace=<pixel trace> | expires 2026-09-08` written the **same
second**; Redis set updated ≤3s; the "Cart Retargeting" campaign then won
the auction for that user on the dog blog.

## The fan-in — one writer between Postgres and the bid path

This is the clever bit, and the part worth defending in review. Migration
078 puts an AFTER INSERT/DELETE trigger on `audience_segment_members`: every
write — batch, real-time, upload, GDPR purge, physical TTL sweep — appends
`(account, user, segment, visibility, add|remove)` to
`audience_membership_changelog` **in the same transaction**. There is no
"remember to invalidate" anywhere; correctness rides the write itself.

One drainer in `cmd/pipeline` (single-replica service, drain/reconcile
mutex-serialized) does everything Redis-side:

- **Delta drain**, every `audience.changelog_poll_interval` (3s): read rows
  past the watermark (batches of 1000), SADD/SREM
  `audience:set:{user_id}:{visibility}`, persist the watermark to Redis
  (`audience:changelog:watermark` — Redis-sourced per drain so a FLUSHDB
  resets the cursor with the data), then DELETE the consumed rows — the
  outbox stays bounded.
- **Reconcile**, every `audience.changelog_reconcile_interval` (5m): scan
  the full truth (`WHERE expires_at IS NULL OR > now()` — TTL enforced
  read-side), atomic `ReplaceSet` per changed key, and **tombstone** keys
  that existed last reconcile but vanished (prev-keys diff) — without that,
  a purged/expired user would be targeted from a stale set forever.
- **Waits for Redis at boot** — it must never run against the SelfHealingL2
  memory fallback, or writes would be silently lost.

Readers are trivial by design: `pkg/audience/store/preload` is a read-only
`SMEMBERS` per lookup, no loops, no subscriptions, degrade-to-empty. The DSP
unions the SSP-stamped public segments with its own `dsp_private` lookups
(user + household, concurrent, one shared 25ms budget — a slow lookup
degrades the refinement, never the bid). The SSP stamps public segments on
outbound requests and attaches IAB taxonomy (`user.data`, segtax=4) for
labelled public segments, consent-gated, with data-fee attribution on
external wins.

Observed: watermark seq 19→(57 enrollments drained); gauges on the pipeline
`/metrics`: `adtech_audience_cache_changelog_backlog 0`,
`..._lag_seconds 0`, `..._drain_age_seconds 0.93`. Grafana Overview has a
panel; WARN log ("consider sharding the writer") at
`audience.changelog_lag_warn` (30s).

## Exits and refinements

- **TTL aging** (mig 073): retargeting members carry `expires_at`; both read
  paths and the reconciler exclude expired rows, so expiry is correct even
  before the physical purge (audience-rt ticker, 60s) deletes them.
- **GDPR purge**: privacy-delete (chain step) deletes the user's member rows
  cross-tenant; the same trigger/changelog/reconcile machinery propagates
  the removal to Redis. Level-3 deletion also purges ClickHouse signals.
- **Derived segments**: composite and lookalike run *after* behavioural
  rules each batch (account-scoped: a composite can only reference its own
  account's segments).
- **Segtax/data fees**: public segments with IAB Audience Taxonomy labels
  ride to external bidders as `user.data` and accrue data fees per delivered
  impression, billed to the trusted settlement seat.

## Operational crib sheet

| What | Where |
|---|---|
| Backlog / drain lag / liveness | pipeline `/metrics` `adtech_audience_cache_*`; Grafana Overview panel |
| Force a drain+reconcile | pipeline `/debug/audience/refresh` (synchronous, 60s cap) |
| Membership counts per segment | `SELECT s.name, count(*) FROM audience_segment_members m JOIN audience_segments s ... GROUP BY 1` (no UI today — judgement-pass item) |
| A member's provenance | `source` + `origin_trace` on the member row (`ing_*` = upload job, pixel trace = audience-rt, `batch_*` = builder run) |
| Batch chain health | staff portal → Batch runs (`batch_runs` table, 5s poll) |
| Ingest job state | staff onboarding monitor; `audience_ingest_jobs` rows |
| Knobs | `audience.changelog_poll_interval` 3s · `changelog_reconcile_interval` 5m · `changelog_lag_warn` 30s · `audience.cache_ttl` 15m · `audience_rt.purge_interval` 60s · `pipeline.ingest_worker_interval` 5s |

Gotchas that bit during the walkthrough (kept so they don't bite you):
household freq caps saturate fast when hand-curling from one IP — use the
allowlisted `?ip=` override; per-user-per-campaign caps mean a repeat serve
for the same user goes no-fill even though the DSP bid (check `dsp_calls`
for the trace before blaming targeting); the DSP's campaign warm cache picks
up reseeded campaigns on its bulk-refresh tick, not instantly.

## Cadence policy

Current: behaviour rules recompute **hourly** (conductor chain), changelog
drain **3s**, reconcile **5m**, TTL purge **60s**. Phase 3 of handoff 07
loads the pipeline at production density (synthetic tier + audience-predicate
big-world campaigns) and will record deliberate choices here — including
whether frequency rules deserve a 15m tier and whether 3s/5m survive
O(100k-1M) memberships.
