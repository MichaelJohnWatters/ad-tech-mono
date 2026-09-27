# Learning Path — Know Your Own System Cold

A structured curriculum to internalize this platform well enough to **demo it
live and defend it in interviews**. It leans on the docs and per-directory
`CLAUDE.md` files you already have, plus guided "trace one request" tours through
the real code.

> **How to use this:** work top-to-bottom. Each module has **Read → Do → Prove**.
> Don't just read — the "Do" (run it, trace it) and "Prove" (say it out loud
> without looking) are where learning actually happens. Budget ~3 weeks part-time.

---

## Learning principles (don't skip)

1. **Follow the data, not the file tree.** You learn a system by tracing one
   request end-to-end, not by reading directories alphabetically. Modules 2–6 are
   built as traces for this reason.
2. **Tests are the truth.** When a doc and the code disagree, the passing test
   wins. Each module names the best test to read — read it as a spec.
3. **Active recall beats re-reading.** After each module, close the laptop and
   explain the flow to a rubber duck (or me). If you can't, you haven't learned it
   yet — go back to the one stop you got stuck on.
4. **Always ask "why this way?"** Every module has a *trade-off* worth being able
   to defend — those are collected in Module 10. Interviewers probe decisions, not
   feature lists.
5. **Teach-back is the mastery gate.** You know a module when you can whiteboard
   it from memory and answer the "interview angle" questions cold.

### Use me (Claude) as your tutor

At any point, in this repo, ask:
- *"Explain `pkg/auction/strategy.go` `RunAuction()` to me like I'm ramping onto the team."*
- *"Quiz me on the data-onboarding flow — 5 questions, hardest first, then grade my answers."*
- *"I said X about how pacing reconciles — is that right? Where's the code?"*
- *"Play the interviewer: 'design an RTB exchange.' Push back on my answers."*
- *"Walk me through `tests/e2e/tracker_test.go` line by line."*

The teach-back loop (you explain → I catch gaps → you re-read the one stop you
missed) is the fastest way through this.

---

## Suggested schedule (~3 weeks, ~1 hr/day)

| Day | Module |
|---|---|
| 1 | 0 — Orientation: the map, the vocabulary, the data model |
| 2 | 1 — Run it & watch it move |
| 3–4 | 2 — Trace a bid request end-to-end (the spine) + fraud, ads.txt, deals |
| 5 | 3 — The money loop |
| 6–8 | 4 — **Your domain: data onboarding & identity** (go deep) |
| 9 | 5 — Attribution & conversion measurement |
| 10–11 | 6 — **Video, SSAI & CTV** (your ad-tech specialty) |
| 12 | 7 — Cross-cutting: tenancy/RLS, caching, security, config, events |
| 13 | 8 — Reporting, analytics & debugging with observability |
| 14 | 9 — Ops & scale (k8s, perf, limits) |
| 15 | 10 — Design decisions & trade-offs (the "why") |
| 16–21 | 11 — Teach-back, war stories & mock interviews |

---

## Module 0 — Orientation: the map, the vocabulary, the data model

**Goal:** hold the whole system in your head as ~6 boxes, know the words, and
know the core tables.

**Read**
- `CLAUDE.md` (root) — the stack, conventions, the design-decision table.
- `docs/PLAN.md` — read the **table of contents** and the top of each major
  section. Don't try to absorb it all; build the skeleton.
- `docs/diagrams/README.md` + skim the SVGs — the visual system map.

**Do**
- On one sheet of paper, draw the six boxes and the arrows between them:
  **SSP → Exchange → DSP** (bid), **Exchange → AdServer → Tracker** (serve/track),
  **Tracker → NATS → Reporting → Billing** (money), and the **data pipeline**
  feeding audiences into the DSP/SSP.
- Sketch the **core data model** from `migrations/`: the campaign hierarchy
  (**Insertion Order > Line Item > Creative**, and the key quirk that
  `campaign_id` = the line-item id *everywhere*), `accounts` (account_id +
  account_type + residency_region), the audience tables (`audience_segments`,
  `audience_segment_members`, `audience_membership_changelog`), `identity_graph` /
  `identity_clusters`, and the money tables (`campaign_committed_spend`,
  reservations, the ledger).

**Prove** — define, in one sentence each, without looking: SSP, DSP, exchange,
OpenRTB, bid request, first-price auction, bid shading, AuctionWinEvent, clearing
price, impression, CPM/CPC/CPA/vCPM, IO/line-item/creative, identity graph,
audience segment, data onboarding, PMP/PG deal. (See the glossary at the end.)

---

## Module 1 — Run it and watch it move

**Goal:** connect the abstract boxes to real screens and logs.

**Read**
- `k8s/CLAUDE.md` → "Dev loop (Makefile)" and "Seed / reset / demo data".

**Do**
- **First:** confirm the stack is healthy — `kubectl get pods -n adtech`. If it
  looks wedged after a host sleep (connection-refused, svclb weirdness), run
  `make stack-doctor` before anything else. If it's down, `make stack-up`.
- `make demo` (seed + traffic), or the `generate-data` skill to fill the portals.
- Open the portals (advertiser, publisher, staff) and the **Trace Explorer** via
  the gateway; open Grafana and Jaeger. Watch auction/fanout latency and fill rate
  move under `make traffic`.

**Prove** — with traffic running, point at a live impression in a dashboard and
narrate every hop it took to get there. If you stall on a hop, that's your next
module.

---

## Module 2 — Trace a bid request end-to-end (the spine)

**Goal:** the single most important flow. Follow it file by file.

**Read the per-service CLAUDE.md first**, then the code stop-by-stop:
`cmd/ssp`, `cmd/exchange`, `cmd/dsp`, `pkg/auction`, `cmd/adserver`, `cmd/tracker`,
plus `pkg/fraud`.

**The tour (open each file, find each function):**
1. `cmd/ssp/main.go` → `serveAuctionRequest()` — request enters; **trace ID minted**
   (`tracing.TraceIDFromContext`, else `ssp-{unixms}`); publisher segments + household
   ID stamped.
2. SSP → Exchange — transport chosen by URL scheme (`grpc://` vs `http://`) via
   `pkg/grpcx` `IsURL()`/`Bridge()`. Understand *why* gRPC internally, OpenRTB externally.
3. `cmd/exchange/main.go` → `auctionHandler()` — auth, decode, **ads.txt check**
   (`fraud.AdsTxtCache`), build internal auction request; deal matching
   (`pkg/deals`) then fan-out via `pkg/auction/router.go` `SmartRouter.Route()`.
4. `cmd/dsp/main.go` → `bidHandler()` — the **hot loop**: per campaign, targeting
   (`pkg/targeting`), budget (in-process counter), bid shading (`pkg/bidshading`).
5. `cmd/dsp/refresh.go` → `startBidCacheRefresher()` — **why the hot loop does no
   I/O**: background bulk refresh of budget/balance from Redis. This is your
   "hot-path iron rule" story.
6. `pkg/auction/strategy.go` → `Engine.RunAuction()` — `SelectStrategy()` by
   channel, floor filter, first-price select. Skim the strategy files
   (`single.go`, `pods.go`, `relevance.go`).
7. `cmd/exchange/main.go` → `pub.AuctionWin(...)` — **AuctionWinEvent published**,
   the single source of truth for cost, deduped by `Nats-Msg-Id = "win:"+trace`.
8. `cmd/adserver/main.go` — serve creative with HMAC-signed tracking URLs + freq cap.
9. `cmd/tracker/main.go` → `/v1/t/imp` — validate HMAC, **real-time fraud check**
   (`pkg/fraud/realtime.go`: bot UA, IP blocklist, rate limiting), publish
   `adtech.events.impression` (returns the 1×1 GIF *before* the ack — why?).

**Detour — deals & the supply path (industry-relevant):** read `pkg/deals` and
`docs/PLAN.md` → "Deal Management". Know the priority order **Programmatic
Guaranteed > Preferred Deal > PMP > Open Auction**, and that a matched guaranteed
deal short-circuits the open auction. This is core sell-side/curation territory.

**Detour — fraud & traffic quality:** `pkg/fraud` — ads.txt verification (before
accepting a request) vs real-time IVT checks (on the tracker path). Know where
each runs and why fraud is checked at *both* the front door and the pixel.

**Best test to read:** `tests/e2e/tracker_test.go`.

**Prove** — whiteboard the 9 stops from memory. Then answer:
- Why gRPC internally but OpenRTB externally?
- What exactly must never happen inside the DSP bid loop, and how is that enforced?
- Why is the clearing price from the *exchange* the source of truth, not the DSP's bid?
- Where does a PMP deal change the flow, and why does it win over the open auction?

---

## Module 3 — The money loop

**Goal:** how a win becomes billed spend, provably without loss.

**Read:** `pkg/billing/CLAUDE.md`, then `pkg/billing/billing.go` `ProcessEvent()`;
`cmd/reporting/main.go` `handleImpressionEvent()`; `cmd/dsp/pacing_reconcile.go`.

**Concepts to nail**
- **Reserve/settle:** CPM bills on impression; CPC/CPA *reserve* on impression,
  *settle* on click/conversion (`SettleByTrace`), release if unsettled past window.
- **Clearing price is per-impression dollars, not CPM** — normalise on read.
- **Two-meter pacing:** DSP's local Redis counter over-counts fast; reporting
  broadcasts the authoritative billed snapshot; DSP reconciles to it.
- **The VERIFY invariant:** money-in = money-out ± $0.01; exactly-once via
  `Nats-Msg-Id` + business-key dedup.

**Prove** — explain reserve/settle for a CPA campaign end-to-end, and explain how
you'd *prove* to a skeptic that no money leaked (the VERIFY step + soak test).

---

## Module 4 — YOUR DOMAIN: data onboarding → identity → activation

**Goal:** the flow that maps to your ad-tech background. Over-invest here — it's
your interview edge and the demo's hero path.

**Read:** `cmd/pipeline/CLAUDE.md`, `cmd/profile-builder/CLAUDE.md`,
`cmd/audience-rt/CLAUDE.md`, `cmd/identity-consumer/CLAUDE.md`,
`pkg/identity/CLAUDE.md`, `pkg/audience/CLAUDE.md`, `pkg/privacy/CLAUDE.md`, plus
`docs/AUDIENCE-PIPELINE.md`.

**The tour:**
1. **Ingest** — `cmd/pipeline/ingest_worker.go` (drop-zone poll) →
   `pkg/ingest/processor.go` `Process()`: decrypt, sniff format, validate
   all-or-nothing, then `Audience.UpsertSegment()` + `AddMembers()` with lineage
   (`origin_trace` = job id).
2. **The append-only changelog** — `migrations/078_*` trigger appends every
   membership change *in the same transaction*. Understand why this beats
   "remember to invalidate the cache."
3. **The single drainer** — `cmd/pipeline/audience_cache_writer.go`
   `startAudienceCacheWriter()`: 3 s delta (SADD/SREM) + 5 m reconcile
   (ReplaceSet + tombstone vanished users). A clean distributed-systems story:
   correctness rides the write, one writer, no coordination protocol.
4. **Identity graph** — `cmd/identity-consumer` consumes `adtech.identity.observed`,
   writes deterministic + probabilistic edges; **union-find** clusters ids into
   persons/households.
5. **Profile builder** — `cmd/profile-builder/main.go` → `pkg/profilebuilder`:
   cluster, evaluate behavioural rules (ClickHouse GROUP BY), expand across the
   cluster, reconcile back to members.
6. **Real-time path** — `cmd/audience-rt/main.go`: single-visit rules enroll in
   *seconds* (`AddMembersWithExpiry`); purchase → suppression/burn list, expanded
   to household via the identity graph.
7. **Activation (hot path)** — DSP/SSP read `SMEMBERS audience:set:{user}:{vis}`
   (`pkg/audience/store/preload`). SSP stamps public segments on outbound OpenRTB.
8. **Monetization** — segtax (IAB) labels on outbound; `data_fee_pending` →
   ledger, **billed to the trusted endpoint-bound seat** (not the self-declared
   one). Know why that distinction closes an evasion class.
9. **Clean-room-lite** — `pkg/marketplace` overlap estimate with the
   **100-user minimum-aggregation floor**; the data marketplace's buy/sell +
   CPM-surcharge settlement loop is the money version of this.
10. **Privacy** — `pkg/privacy/consent.go` `Evaluate()` gating capture;
    `cmd/privacy-delete` GDPR purge across PG + ClickHouse + Parquet;
    `pkg/privacy/residency.go` region gate.

**Best test to read:** `tests/e2e/ssp_consent_segments_test.go`.

**Prove** — whiteboard onboard-to-activation from memory. Then:
- A customer uploads a hashed-email list. Trace it to a won auction. Where's the
  identity resolution, and what's deterministic vs probabilistic?
- How does activation latency differ for a "visited 3× in 7 days" rule vs a
  "visited once" rule, and why?
- How do two advertisers estimate audience overlap **without** either seeing the
  other's users? (min-agg floor + what it protects against.)
- Where does consent stop data from being captured, and where does GDPR delete it?

---

## Module 5 — Attribution & conversion measurement

**Goal:** how a conversion gets tied back to the ad that caused it — the
measurement half of the business, and a classic ad-tech interview topic.

**Read:** `pkg/attribution/CLAUDE.md` + `pkg/attribution/`; `cmd/tracker`
(`/v1/t/click`, `/v1/t/conv`, `/v1/t/view`); plans in `docs/attribution-plan.md`,
`docs/attribution-gaps-plan.md`, `docs/attribution-phase4-ara.md`.

**Concepts to nail**
- **Click-through:** a signed click id (`ctid`) minted at redirect; the conversion
  pixel presents it back. Signing prevents forged click credit.
- **View-through & cross-device:** no click — resolve the converting user to the
  exposed user via the **identity graph** (confidence-floored), across devices.
- **The identity bridge:** the retargeting/measurement pixel builds identity edges
  that later attribution resolves against.
- **Multi-touch:** multiple exposures → the reporting layer distributes credit.
- **Per-advertiser conversion signing keys** (mig 071): `/v1/t/conv` validated by
  advertiser id → closes **cross-advertiser CPA forgery**; strict flag
  `tracker.conversion_strict_advertiser_key`; self-serve `POST /v1/api/conversion-key`.
- **Privacy Sandbox ARA (Phase 4):** the privacy-preserving, aggregate,
  cookieless attribution path — the on-trend answer to cookie deprecation, and a
  direct tie-in to your data-onboarding/privacy story.
- **Gotcha:** there are *two* conversion consumers; the batch one in reporting is
  the active biller. Know which settles the money (links back to Module 3).

**Prove** — explain click-through vs view-through vs cross-device attribution, and
how each is made forgery-resistant. Explain what ARA changes and why the industry
needs it.

---

## Module 6 — Video, SSAI & CTV (your ad-tech specialty)

**Goal:** the video/CTV stack — the part of your résumé's ad-tech experience
loudest. Own it.

**Read:** `cmd/ssai` + `cmd/transcoder/CLAUDE.md`; `pkg/auction`'s pod strategy
(`pods.go`); `docs/PLAN.md` → "Video Ads, SSAI, and CTV".

**Concepts to nail**
- **SSAI (server-side ad insertion):** the ad is stitched into the video
  **manifest** server-side (HLS `#EXT-X-CUE-OUT` breaks / DASH periods), so the
  ad segments are indistinguishable from content — ad-blocker-proof, the CTV norm.
- **Transcoding:** `cmd/transcoder` builds an ABR (adaptive bitrate) ladder with
  ffmpeg; segments are pre-conditioned and cached so stitching is fast.
- **Per-break pod auctions:** each ad break (avail) runs its own auction for up to
  `ssai.max_pod_ads` slots, with competitive separation inside the pod.
- **VAST beacons fired server-side** (HMAC-signed), including quartiles — because
  there's no browser JS executing them.
- **The freq-cap PEEK/RECORD split** — the subtle, real bug you fixed: the SSP
  *peeks* the cap during the auction (`cap_defer=1`) but the stitcher only
  *records* when an ad is actually stitched. Counting on the serve *decision*
  over-counts and loses fill. This is a great "I found a non-obvious correctness
  bug" story.
- **Slate** on no-fill/error so the stream never breaks.

**Best test to read:** the SSAI e2e (`tests/e2e/` — the SSAI stitch test).

**Prove** — explain why CTV uses server-side stitching instead of client-side ad
calls, and walk the PEEK/RECORD cap split and the bug it fixes. Explain how a pod
auction differs from a single-slot display auction.

---

## Module 7 — Cross-cutting mechanics

**Goal:** the machinery every service shares. Short tours.

**Multi-tenancy / RLS** — `pkg/middleware/auth.go` `Auth()` + `scope.go`
`CallerScope()`; `pkg/store/postgres/postgres.go` `WithTenant()` /
`QueryPlatform()`; example policy in `migrations/019_*` (the
`account_id = NULLIF(current_setting(...),'')::uuid OR platform_read` shape).
**Key fact:** an unscoped query on an RLS table returns **0 rows silently** — this
caught real bugs. Great "defence in depth" story.

**RBAC & auth surfaces** — the permission model is `resource:action` (e.g.
`marketplace:buy`, `support:update`) gated by **account type** + role; a
wrong-role/wrong-type caller gets **403**. Sessions (cookies) for the portal, API
keys (`X-API-Key`) for partners, OIDC SSO for enterprise. This is the enforcement
catalog in `docs/SECURITY.md` — worth skimming.

**Security mechanics you should be able to name** — HMAC signing of
beacons/tracking URLs (`ValidateSignatureAny`); **trusted vs self-declared**
(bill the endpoint-bound seat, never the caller's claimed seat);
`middleware.StripClientIdentityHeaders` (the CORS pass-through fix from the audit);
body caps + id validation → 4xx not 500. See `docs/SECURITY-AUDIT-PROMPT.md`.

**Caching tiers** — `pkg/cache/CLAUDE.md`: **L1** in-process Go maps (read-heavy
config), **L2** Redis (shared mutable state — budgets, freq caps, audience sets),
**L3** Postgres (source of truth). Invalidation via Core NATS pub/sub
(`adtech.cache.invalidate.*`), self-healing L2. This is the read-path story that
makes the hot loop fast — pairs with Module 2's "no I/O in the bid loop."

**Config tiers** — `pkg/config/setup.go` `Setup()`, typed handles in
`pkg/config/keys/*`. Resolution: pod row > global row > env > default (pod rows
*shadow* global — a real gotcha).

**Events** — `pkg/events/events.go` (`EventBus`), `publisher.go` (typed helpers +
`Nats-Msg-Id`), `subjects.go` (catalog). Rules: at-least-once → handlers
idempotent; exactly-once = stable msg id + dedup key; cache invalidation uses
*ephemeral broadcast* consumers (never per-pod durables — they wedge JetStream).

**Prove** — explain the 3-layer tenant defence (gateway → service → RLS) and why
RLS-returns-0-rows is a feature; the L1/L2/L3 cache tiers and what lives where;
why you bill the trusted seat; how you get exactly-once from an at-least-once bus.

---

## Module 8 — Reporting, analytics & debugging with observability

**Goal:** how events become queryable numbers, and how you *operate* the system.

**Read:** `cmd/reporting/CLAUDE.md`, `pkg/reporting/CLAUDE.md`; the rollup docs in
`docs/PLAN.md`.

**Analytics concepts:** ClickHouse hot store (<1 h) + Parquet-on-S3 cold, queried
through one interface via ClickHouse `s3()`; rollup tiers (hour/day/month)
auto-selected by query range; the completion-ordered batch chain
(`cmd/batch-conductor`).

**Debugging with observability** — this is a strong "how I operate" interview
story:
- **Trace Explorer** (portal) — `analytics.TraceReader`, `/v1/reporting/trace`:
  the full timeline of one request across services (waterfall + logs + counts).
- **Jaeger** (spans/latency), **Loki** (logs by `trace_id`), **Grafana/Prometheus**
  (fleet metrics).
- The **`debug-nobid` skill** — top-down: "why didn't this advertiser win?" from
  the exchange routing decision down to the per-campaign eligibility gate.

**Best skill/test:** the `verify-pipeline` skill; `tests/e2e/hotcold_test.go`.

**Prove** — a user asks for a report spanning last hour + last year: what stores
answer it, and how is that one query interface? Then: "an advertiser says they're
not winning any auctions — how do you diagnose it?" (walk the debug-nobid path).

---

## Module 9 — Ops & scale (the honest limits)

**Goal:** run it, and speak credibly about where it breaks.

**Read:** `k8s/CLAUDE.md`; the `perf-loadtest` skill (`.claude/skills/perf-loadtest/`).

**Facts to own**
- Deploy: Rancher Desktop k3s + Helm; `make stack-up` / `deploy SVC=x` / `reset`;
  `make stack-doctor` for post-sleep wedges.
- Numbers: ~180 req/s lossless on 8 vCPU; auction p95 ~73 ms; per-campaign eval
  p95 <1 ms; the 8-vCPU ceiling is a VM/CPU limit, not an algorithmic one.
- The scaling story: to go 10–100×, shard the bid loop, colocate DSPs with the
  exchange, keep the hot path GC-quiet, and scale trackers horizontally (a single
  tracker's readiness-flap under load = the ~1% slippage you measured).

**Prove** — "This does 180 rps; a real exchange does millions. What do you change?"
Answer it structurally (shard, colocate, GC, horizontal trackers), not hand-wavily.

---

## Module 10 — Design decisions & trade-offs (the "why")

**Goal:** interviewers probe *decisions*. For each below, be able to state the
choice, the alternative, and why you'd defend it. Source: `docs/PLAN.md` design
table + `CLAUDE.md`.

- **First-price auction + bid shading** (vs second-price) — why the market moved,
  and what shading solves.
- **The `AuctionWinEvent` as single source of truth for cost** — why one event,
  published once by the exchange, instead of trusting the DSP's bid or the tracker.
- **gRPC on owned internal edges, OpenRTB/HTTP everywhere external** — why not one
  transport everywhere.
- **Hot/cold (ClickHouse + Parquet) behind one interface** — why not a single store.
- **RLS as fail-safe defence-in-depth** (0 rows on unscoped query) — why belt *and*
  braces over "just filter in code."
- **Append-only changelog + single drainer** (vs fan-out cache invalidation) — why
  correctness-rides-the-write beats "remember to invalidate."
- **No I/O in the hot loop + background bulk refresh** — the latency/consistency
  trade (you serve slightly stale budgets to never block a bid).
- **`campaign_id` = line-item id** — the modeling shortcut and what it costs.
- **Reserve/settle for CPC/CPA** — why you can't just bill on the impression.
- **Bill the trusted endpoint-bound seat, not the declared seat** — the integrity
  trade-off.

**Prove** — pick any three at random and defend them against "why not the other
way?" out loud.

---

## Module 11 — Teach-back, war stories & mock interviews

**Goal:** convert knowledge into interview performance.

**Do**
- Record yourself giving a **5-minute architecture walkthrough** of the whole
  system. Watch it. Where did you hedge? Re-read that module.
- Do a **3-minute "here's the data-onboarding flow"** version — your opener for
  ad-tech/identity roles — and a **3-minute video/CTV** version for ad-tech SSP roles.
- Ask me to run mock rounds: *"Play a staff engineer interviewing me. Prompt:
  'design an identity onboarding + audience activation pipeline.' Use my system as
  the reference; push on privacy, scale, and correctness."*

### War stories — prep 5–6 as STAR (situation → task → action → result)

"Tell me about a hard bug / a big perf win" is guaranteed. You have gold — for
each, know the symptom, the *diagnosis method*, the fix, and the number:

- **The auction-speed push** — bid loop p95 68→1 ms by taking Redis out of the
  hot path; the root cause was a go-redis deadline leak (`ContextTimeoutEnabled`)
  so the 25 ms caps never bound. Perf + latency-discipline story.
- **NATS deaf-on-boot** — a consumer that fails `Subscribe` once at boot logs
  success but goes deaf forever; found 5, all retry-until-stick now. "Silent
  failure" story; caught by running e2e against the live stack.
- **NATS consumer leak** — per-pod *durable* cache-invalidate consumers leaked and
  wedged JetStream (1169 consumers); fix = ephemeral broadcast. Distributed-systems
  story.
- **The RLS silent no-op** — a bare-pool query on an RLS table under `adtech_app`
  returns 0 rows silently; the DB-role flip made this real. "Fail-safe security
  bit me, then saved me" story.
- **VM memory thrash** — 200 qps collapsed to 135 rps from 12 GiB VM page-cache
  thrash; fixed to 177.9 rps lossless. Systems-perf-under-load story.
- **The exactly-once money proof** — 96,778/96,778 events via `Nats-Msg-Id` +
  business-key dedup. Correctness-you-can-prove story.

**Prove** — deliver two war stories cold, in under 2 minutes each, ending on the
number.

---

## Interview question bank (practice cold)

**System design**
- Design an RTB exchange. (Walk fan-out, timeout, auction, win-event.)
- Design a first-party data onboarding + audience activation pipeline. (Your edge.)
- Design server-side ad insertion for CTV. (Your ad-tech edge.)
- Design a conversion attribution system that resists forgery.
- How do you guarantee you never bill an advertiser twice for one impression?
- How do two parties compute audience overlap without sharing users?

**Deep-dive (know the file)**
- Why no per-call I/O in the bid loop, and how is it enforced? (`cmd/dsp/refresh.go`)
- How does audience membership reach the bid loop within 3 s without a fan-out
  invalidate storm? (changelog trigger + single drainer)
- What happens if NATS is down when a win fires? (spool + dedup window)
- Why does an unscoped query return 0 rows instead of everything? (RLS fail-safe)
- Walk the SSAI freq-cap PEEK/RECORD split and the bug it fixed.

**Domain (industry-adjacent)**
- Deterministic vs probabilistic identity — when do you use each, and the
  confidence model?
- How do you keep consent flowing through the whole chain, and prove a
  revoked-consent user's data is gone?
- Data fees: why bill the endpoint-bound seat and not the declared seat?
- Deal priority: why does PG beat PMP beats open auction?
- What is Privacy Sandbox ARA solving, and how does it change measurement?

**Trade-offs (Module 10)**
- Why first-price with shading instead of second-price?
- Why is the win event the single source of truth for cost?
- Why hot/cold instead of one analytics store?

---

## Glossary (quick reference)

- **SSP / DSP / Exchange** — sell-side (publisher), buy-side (advertiser), the
  auctioneer in the middle.
- **OpenRTB** — the industry JSON/HTTP protocol for bid requests/responses.
- **First-price auction** — winner pays their bid (not the runner-up's); needs
  **bid shading** (bidding below true value to avoid overpaying).
- **Clearing price** — the realized per-impression cost from the auction.
- **AuctionWinEvent** — the one event, published by the exchange, that is the
  source of truth for cost.
- **CPM / CPC / CPA / vCPM** — cost per mille (1000 impressions) / click /
  action(conversion) / viewable mille.
- **IO / Line Item / Creative** — campaign hierarchy; `campaign_id` = line-item id.
- **Reserve / Settle** — hold budget on impression (CPC/CPA), commit it on the
  downstream click/conversion.
- **Identity graph** — edges linking a person's ids across devices; clustered via
  union-find into persons/households.
- **Audience segment** — a set of users; onboarded (uploaded), behavioural (rule),
  or retargeting (pixel).
- **Data onboarding** — turning first-party/offline data into resolved,
  activatable audiences (privacy-safely).
- **PMP / PG / Preferred Deal** — private marketplace / programmatic guaranteed /
  preferred; deal types with priority over the open auction.
- **SSAI** — server-side ad insertion; stitching ads into the video manifest.
- **VAST** — the XML spec for video ad responses + tracking beacons.
- **ARA** — Privacy Sandbox Attribution Reporting API; privacy-preserving,
  aggregate, cookieless attribution.
- **RLS** — Postgres Row-Level Security; per-tenant row filtering at the DB.
- **segtax** — segment taxonomy (IAB); labels that ride to bidders as `user.data`.

---

## Mastery checklist

- [ ] I can whiteboard all 6 boxes and the arrows from memory.
- [ ] I can sketch the core data model (campaign hierarchy, audience, identity, money).
- [ ] I can trace a bid request through 9 stops naming the real functions.
- [ ] I can explain reserve/settle and how money-loss is *proven* absent.
- [ ] I can give the 3-minute data-onboarding → activation walkthrough cold.
- [ ] I can explain click / view / cross-device attribution and how each resists forgery.
- [ ] I can explain SSAI + the PEEK/RECORD cap split (the video/CTV flex).
- [ ] I can explain the changelog+drainer design and why it needs no coordination.
- [ ] I can explain the 3-layer tenant defence, RLS-0-rows, and the L1/L2/L3 caches.
- [ ] I can defend three design trade-offs against "why not the other way?"
- [ ] I can deliver two war stories cold in under 2 minutes, ending on the number.
- [ ] I can answer "scale it 100×" structurally.
- [ ] I can do all of the above to an interviewer who pushes back.
