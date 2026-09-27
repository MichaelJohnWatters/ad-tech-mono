# Building a Transparent Programmatic Ad Platform, Solo

*A full SSP → exchange → DSP → ad-serving stack with a first-party
data-onboarding pipeline, an identity graph, and an exactly-once money ledger —
built end-to-end in Go, running on Kubernetes, traceable from bid to dollar.*

> **Links:** live demo → `<demo-url>` · source → `<repo-url>`
> **Author:** <your name> — backend/platform engineer, with prior ad-tech industry experience.

---

## Why I built this

I worked on **data onboarding at a prior ad-tech employer** — the problem of taking a company's
first-party data, resolving it to real people, and turning it into audiences you
can actually activate. That's one slice of a much larger machine, and I wanted to
understand — and be able to build — *the whole thing*: how a bid request becomes
an auction, a win, a served ad, a tracked impression, an attributed conversion,
and finally a line on an invoice, with the audience data I used to work on wired
through the middle of it.

So I built the entire platform, solo, with one guiding constraint: **total
transparency — you can trace any ad request end-to-end with zero data slippage.**
The industry's biggest problem is opacity; I wanted to prove a stack could be
fully accountable, down to a ±1¢ money invariant.

It's ~170K lines of Go across 18 services, and it runs on a local Kubernetes
cluster with one command.

---

## What it is

A complete programmatic advertising platform — the pieces that are usually five
different companies, in one monorepo:

- **SSP** — publisher inventory, floors, quality controls; mints the trace ID.
- **Exchange** — fans out bid requests (gRPC to owned DSPs, OpenRTB/HTTP to
  external), runs first-price auctions with deal priority, publishes the
  authoritative win event.
- **DSP** — campaign/budget management, pacing, bid shading; a hot bid loop with
  no per-call network I/O.
- **Ad server + SSAI** — creative serving, frequency capping, and server-side ad
  insertion into HLS/DASH video manifests.
- **Tracker** — internet-facing impression/click/conversion pixels, HMAC-verified,
  <10 ms.
- **Data / identity pipeline** — the part I care most about (below).
- **Reporting + billing** — ClickHouse hot store, Parquet cold archive, an
  exactly-once ledger, invoicing and publisher payouts.

Plus the operational reality: multi-tenancy with row-level security, SSO,
consent/GDPR, a data marketplace, and full observability (Prometheus, Grafana,
Loki, Jaeger).

---

## The part I'm proudest of: data onboarding → activation, end to end

This is the flow that maps to my background, and it's the demo's hero path:

1. **Ingest.** A first-party customer list lands via API or an S3 drop-zone
   (`onboarding/{provider}/incoming/`). The pipeline PGP-decrypts, sniffs the
   format, maps it to the tenant, and validates all-or-nothing.
2. **Hash & resolve.** PII is hashed; identifiers feed an **identity graph** —
   deterministic edges (shared IDs = same person, full confidence) plus opt-in
   probabilistic edges. A **union-find** pass clusters IDs into people and
   households.
3. **Segment.** Members become audience segments. Behavioural segments are
   recomputed hourly from consented signals; single-visit retargeting enrolls in
   **seconds** via a dedicated real-time consumer — same store, faster path.
4. **Activate.** Segments sync to Redis through an **append-only membership
   changelog** (a Postgres trigger writes it in the same transaction as the
   membership change; one drainer applies it — no cache-invalidation races). The
   bid loop reads them with a single `SMEMBERS`.
5. **Monetize.** Public segments ride outbound OpenRTB as IAB-taxonomy
   (`segtax=4`) `user.data`, consent-gated. When an external buyer wins on that
   data, a **data fee** is billed — to the *trusted, endpoint-bound seat*, not the
   caller's self-declared seat (closes an evasion class) — and settled to the data
   owner's balance.
6. **Collaborate, privately.** A clean-room-style overlap/expansion estimate
   suppresses any result under a **100-user minimum-aggregation floor**.

Wired through all of it: **consent gating at capture time**, **per-account data
residency**, **GDPR purge** across Postgres + ClickHouse + Parquet, and
**lineage** — every segment member traces back to the exact source file and
ingest job that created it.

---

## Engineering highlights

**Exactly-once money.** One `AuctionWinEvent` is the single source of truth for
cost. Spend uses a reserve/settle pattern (CPM bills on win; CPC/CPA reserve on
win, settle on the downstream event). A load-tested invariant proves
**money-in = money-out to ±$0.01 over 30-minute soaks**, with exactly-once event
delivery (message-id + business-key dedup) verified at 96,778/96,778 events.

**No I/O in the hot path.** The per-campaign bid loop does zero per-call network
calls — everything it needs is an in-process copy kept warm by background bulk
refreshers. Result: **auction p95 ~73 ms, per-campaign eval p95 <1 ms, ~180 req/s
sustained lossless on 8 vCPU.**

**Multi-tenancy that fails safe.** Every tenant-scoped query filters by
`account_id`, backed by PostgreSQL **Row-Level Security** as a third defence layer
behind gateway and service auth. RLS silently returns 0 rows on an unscoped query
— which caught real bugs the application layer missed.

**Traceable end to end.** A single 32-hex trace ID follows a request from bid →
win → serve → pixel → NATS → reporting → billing. A **verify step** in the batch
chain cross-checks event counts at every stage and fails the pipeline on any
slippage — the transparency guarantee, enforced in code.

**Hot/cold analytics.** ClickHouse serves sub-second queries on recent data;
deep history lives in Parquet on S3 and is queried through the same interface via
ClickHouse's `s3()` function. Rollup tiers (hour/day/month) are auto-selected by
query range.

---

## Tech stack

- **Language:** Go (everything). Python only for ML/data-science bits.
- **Protocols:** gRPC on owned internal hot edges, OpenRTB 2.6 over HTTP on every
  external bidding boundary, NATS JetStream for async events, HTTP/JSON for the
  dashboard and pixels.
- **Data:** PostgreSQL (transactional, RLS), ClickHouse (hot analytics),
  Parquet + Delta on S3 (cold), Redis (L2 cache), TigerBeetle (ledger).
- **Infra:** Kubernetes (Rancher Desktop k3s locally), Helm chart, one-command
  deploy / seed / reset.
- **Observability:** slog structured logging, Prometheus + Grafana, Loki, Jaeger.
- **Frontend:** Go templates + HTMX + Tailwind — no JS build pipeline.

---

## What's real vs. simulated (honesty matters)

- **Real:** the auction, serving, tracking, identity, attribution, reporting, and
  money paths all run for real against live Postgres/ClickHouse/Redis/NATS/S3. The
  money invariant and event-count verification are enforced, not decorative.
- **Simulated:** traffic is generated by a persona-driven simulator that goes
  through the *real* serving path (real bid requests, real auctions, real
  impressions). External DSPs and an external publisher are stand-ins so the loop
  closes locally.
- **Scale caveat:** this runs at ~180 req/s on a single 8-vCPU node. A production
  exchange does orders of magnitude more; the interesting question is how you'd
  shard the bid loop, colocate DSPs, and keep the hot path GC-quiet to get there —
  happy to talk through it.

---

## Try it

- **Live demo:** `<demo-url>` — log in as an advertiser, a publisher, or the
  platform operator; watch live auctions; or trace a single bid request end to end
  in the Trace Explorer.
- **Architecture deep-dive:** see `docs/PLAN.md` and the diagrams in
  `docs/diagrams/`.
