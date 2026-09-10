# Programmatic Ad Platform - Project Plan

## Navigation Guide

This document is 13,000+ lines. Use this legend to find what you need.

### Architecture & Overview
- **Vision** - what we're building and why
- **Ad Tech Core Components** - DSP, SSP, Exchange, Ad Server, Tracker, Reporting, etc.
- **System Diagrams** - Mermaid + D2 architecture diagrams, sequence diagrams, ER diagrams
- **Technical Approach** - tech stack summary table
- **Design Principles** - 7 core principles (traceable, runnable locally, one language, etc.)
- **Monorepo Structure** - full directory tree (`cmd/`, `pkg/`, `k8s/`, etc.)
- **What We Are Building (Scope)** - MVP vs deferred features

### Ad Serving & Auction
- **Advertiser Workflows** - campaign lifecycle, creative review, scheduling, budget
- **Publisher Workflows** - deals (PMP/PG/preferred), quality controls, floor prices, inventory forecasting
- **Platform Workflows** - moderation, API tiers
- **Ad Tech Core Features (Expanded)** - 16 detailed features (#1-16): first-price auction, bid shading, campaign hierarchy, frequency capping, targeting exclusions, multi-currency, native ads, OpenRTB app/regs, bid modifiers, third-party pixels, reach/frequency, view-through attribution, competitive separation, macro substitution, revenue share
- **DSP Core Mechanics** - 8 features (#17-24): pacing algorithms, flight management, contextual targeting, sequential messaging, DCO, inventory quality scoring, reach forecasting, loss notification feedback loop
- **Unified Auction Engine** - 5 strategies (single winner, pod, relevance-weighted, batch, time-slot), multi-exchange deployment
- **Identity and First-Party Data** - platform ID, publisher/advertiser data, identity graph, privacy, clean rooms
- **Unified Audience Management** - segment types, lookalike audiences, composite segments, unified audience store
- **Clean Rooms and Data Marketplace** - secure computation, expansion estimates, bartering, fairness scoring

### Ad Channels
- **Video Ads, SSAI, and CTV** - VAST/VMAP, SSAI stitcher, transcoder, session manager, live streaming, CTV, CDN, audio normalisation, failover/slate, ABR
- **Audio Ads (Radio and Podcasts)** - DAAST, podcast dynamic insertion, streaming radio
- **Digital Out-of-Home (DOOH)** - screens, proof-of-play, audience estimation, weather targeting
- **Retail Media** - sponsored products, relevance scoring, keyword bidding, closed-loop attribution
- **In-Game Advertising** - rewarded ads, intrinsic billboards, interstitials, reward verification

### API & Contracts
- **Gateway** - single entry point, signup/onboarding, webhooks
- **API Contracts** - gRPC services (all methods), Gateway HTTP endpoints (consolidated), OpenRTB endpoints, Tracker endpoints, NATS subjects (all streams)
- **Authentication, Roles, and Permissions** - 5 account types, role hierarchy, permission model, agency model, SSO
- **API Documentation** - Buf for gRPC, OpenAPI/Swagger for REST
- **Versioning** - API versioning, event/data schema versioning

### Data & Storage
> 📊 **New to the data flow? Start with [`docs/diagrams/data-lifecycle.md`](diagrams/data-lifecycle.md)** — one linear walk (ASCII + Mermaid + rendered [SVG](diagrams/data-lifecycle.svg)) of how data flows from first/third-party + auctions → audience → rollups: normalisation, audience expansion, the seven pixels, the money lane, the privacy/deletion flow, and the serving-vs-analytics boundary.

- **Data Storage** - PostgreSQL, DuckDB/ClickHouse, Minio/S3, Parquet/Delta
- **Caching** - L1/L2/L3, Redis topology, budget handling, cache invalidation
- **Data Pipeline** - ingest, validate, normalise, enrich, schema drift, rollups (universal framework)
- **Database Migrations** - goose, migration files, K8s job

### Operations & Infrastructure
- **Service-to-Service Communication** - gRPC, OpenRTB, NATS JetStream (streams, subjects, ordering, DLQ, abstraction)
- **Container Images** - shared Dockerfile, gateway/reporting/transcoder variants, Tiltfile
- **CI/CD** - GitHub Actions, change detection, image tagging, local canary/A/B testing
- **Infrastructure Plan** - topology, cloud strategy, network, DNS, TLS, resource requirements, QPS targets, storage growth, cost estimates, DR procedures
- **Deployments and Graceful Shutdown** - rolling, canary, A/B, graceful drain, operations UI
- **Deployment Ledger** - permanent deployment history, Grafana annotations, DORA metrics
- **Autoscaling (HPA)** - per-service scaling config
- **Ingress (Traefik)** - routing, TLS
- **Email** - Mailpit local, SES prod
- **Backups and Disaster Recovery** - Postgres WAL, HA everywhere, recovery procedures

### Security & Privacy
- **Security** - SOPS secrets, network policies, application security
- **User Opt-Out and Data Deletion** - 3 levels, deletion map, verification, compliance
- **Fraud Detection and Traffic Quality** - real-time + batch, scoring, ads.txt, IVT
- **ads.txt and sellers.json** - crawler, verification, sellers.json generation

### Observability & Testing
- **Observability** - 3 tiers (slog, Prometheus/Grafana, Jaeger)
- **Log Aggregation** - Loki + Promtail + Grafana
- **Health Checks** - /healthz, /readyz per service
- **Testing Strategy** - 5 layers (unit, integration, e2e, contract, performance)
- **Seed Data and Simulation** - profiles, programmable simulator, k6 performance testing
- **Developer Tools** - Trace Explorer, Publisher Simulator (13 templates for all channels)

### Billing & Finance
- **Billing and Financial Reconciliation** - single source of truth, reserve/settle for CPC/CPA, reconciliation
- **Configuration Management** - live config store, dashboard UI
- **Audit Logging** - what's audited, record structure, storage

### Business Operations
- **Business Operations** - account closure, SDK versioning, status page, support/disputes, API changelog, SSO, double-entry ledger, data residency
- **Production Resilience** - Redis recovery, PgBouncer, DuckDB migration, idempotent consumers, clock abstraction, analytics schema evolution, partner onboarding

### Build Plan
- **Developer Onboarding** - prerequisites, getting started, Makefile targets
- **Next Steps** - 69-step phased build plan across 10 phases

---

## Completed Build: Pod Migration + E2E Green (2026-06-04 → 2026-06-06)

**Status: LIVE. All 9 services run as k8s pods + reporting (local_resource).
E2E suite: 100 PASS / 22 SKIP / 0 FAIL against the podified stack.**

Two sessions of work that moved the dev environment from "host
processes orchestrated by Tilt" to "production-shape pod deployments
under `tilt up`," then drove the e2e suite green against the new
shape. Forms the foundation for any CI work (the failure modes pods
expose — DNS timing, per-pod config rows, in-cluster reachability —
all surfaced and got fixed).

**What shipped:**

- **Pods migration LIVE** — `k8s/base/{tracker,adserver,ssp,exchange,
  publisher-adserver,gateway,dsp}/deployment.yaml` (plus matching
  Services + Ingresses). Reporting stays as `local_resource` because
  DuckDB needs CGO and cross-compile from macOS to Linux/musl was too
  fragile. Iteration loop: edit Go → host cross-compile to
  `./bin/<svc>` (~1-2s) → `docker_build_with_restart` syncs the binary
  into the pod and restarts the process (~1s).
- **Colima must use qemu** — vz vmType crashes the cluster under
  sustained docker-build load on macOS 14. Documented in
  `docs/PODS_MIGRATION.md` + saved memory. Switch requires
  `colima delete --force` + recreate with `--vm-type qemu`.
- **Stable pod IDs** — every Deployment hardcodes
  `POD_NAME=<service>-0` instead of the downward-API random k8s name.
  Tests that target a specific pod's config row
  (`SetConfigForPod(t, key, value, "exchange-0")`) now actually land
  on the running pod. Safe for single-replica dev; prod would use a
  StatefulSet for stable identities across replicas.
- **In-cluster DNS for pod-bound URLs** —
  `tests/e2e/harness/harness.go` gained `URLs.Cluster*` alongside the
  host-side URLs. Bare URL (localhost:8082) is for the test process
  probing services directly; Cluster* (`http://dsp-internal:8082`) is
  for values that pods will dial. localhost from inside a pod is its
  own loopback, not the host.
- **Env-var → DB override at boot** — `pkg/config/setup.go`
  `applyEnvOverridesToDB` overwrites Postgres rows whose value still
  equals the schema default with the env-var value. Without this,
  `registry.Register` seeded e.g. `exchange.dsp_endpoints=localhost`
  (the schema default for local-process mode) and the Postgres value
  beat the in-memory `cfg.SetLive` override on the next poll. Also
  added service-prefixed env-var fallback: deployments set
  `TRACKER_NATS_URL` (not bare `NATS_URL`), so the bridge now reads
  `<UPPER_SERVICE>_NATS_URL` first and propagates to both `nats.url`
  and `tracker.nats_url`.
- **Gateway lazy DB** — `cmd/gateway/main.go` opens the Postgres
  handle once and lets handlers retry on demand. Previously the
  handle was nil-ed if the first Ping failed at boot (postgres DNS
  not ready), and every bootstrap/secrets endpoint 503'd forever.
- **Seed binary baked into gateway image** —
  `build/Dockerfile.dev.gateway` + `cmd/gateway/reset.go` exec `/seed`
  directly instead of shelling out to `go run ./cmd/seed`. The
  gateway pod has no Go toolchain, so the previous reset endpoint
  returned 500 with `exec: "go": executable file not found`.
- **Migration 024 self-bootstraps `service_registry`** — the table is
  normally created lazily by `pkg/config/registry.go` at first pod
  boot, but the migrate job runs before any service starts on a
  fresh cluster. Added `CREATE TABLE IF NOT EXISTS` preamble to 024.
- **Host-bridge for fake DSPs** —
  `tests/e2e/harness/hostproxy.go` `HostReachableServer` binds 0.0.0.0
  and returns a URL with the host IP (Colima's `192.168.5.2`). Pods
  can call back to test-process-hosted fake DSPs/Prebid Servers via
  this. macOS dual-stack listeners report `[::]:port` not
  `0.0.0.0:port` — needed to substitute either form.

**Two new product features fell out of unsticking skipped tests:**

- **Config schema validation** — `pkg/config/manager.SetForPod` now
  consults the registry-published per-pod schema
  (`service_registry.schema_entries`), not just the platform
  `Schema()`. DSP/exchange/tracker etc. each own their config keys;
  without this, `dsp.daily_budget_default` fell through as "unknown
  key, allowed" and the int/float check was bypassed. Gateway PUT
  handler maps `ErrValidation` to 400 instead of 500.
- **Tracker HMAC strict mode actually enforced** —
  `tracker.signature_validation` was declared in the schema but never
  read. Now both impression and viewability handlers return 403 when
  the knob is true and the sig is missing/invalid. Warn-only is still
  the dev default.

**Reporting / ledger flow caught up (un-skipping the CPC/CPA/vCPM
billing tests):**

`cmd/reporting/main.go` now dispatches NATS events into the billing
engine by bid model. `handleClick` calls `Engine.SettleByTrace(…,
"click")` so CPC reservations close on the click event;
`handleConversion` does the same for CPA on conversion;
`handleViewabilityFromTracker` settles vCPM only when the tracker's
server-authoritative `IABViewable=true` lands. CPM stays as
settle-at-impression. See the TigerBeetle "Carry-over follow-ups"
section for which billing-models tests are now active vs which
remain gated on harness helpers.

**Carry-over follow-ups:**

- The 22 still-skipped e2e tests split into: 10 real product gaps
  (privacy not enforced, fraud blocklists not warm-cached, prebid
  multi-imp loop, billing contract-write helpers), 4 chaos tests
  needing `harness.ChaosKill*` helpers (`kubectl delete pod -l app=X`),
  3 observability harness gaps (Jaeger client, log capture, migration
  step helper), 4 billing contract tests needing both contract-write
  helper AND seed data, and 1 Postgres setup (TestRLSIsolation — dev
  role is BYPASSRLS superuser).
- The fundamentally untouched items in the 2026-06-03 backlog
  (opt-out propagation, tracker fraud-rejection events, ad server
  render-fail events, Prebid viewability beacon, auth on CRUD, pubad
  preferred/exclusion) are still queued. See "Next-Up Backlog
  (2026-06-03)" below — order is unchanged.

---

## Completed Build: TigerBeetle-backed Ledger (2026-06-02)

**Status: code-complete; live verification deferred to first `tilt up` with TB pod ready.**

Outcome: TB ledger lives behind a config gate (`billing.ledger_backend`,
default `memory`). 5 phases shipped:

- Phase 1 — `k8s/base/tigerbeetle/` StatefulSet (image
  `ghcr.io/tigerbeetle/tigerbeetle:0.16.43`, init container runs
  `tigerbeetle format`, main runs `tigerbeetle start --addresses=0.0.0.0:3000`,
  1Gi PVC, TCP probes). Tiltfile port-forwards 3033 → 3000.
- Phase 2 — `pkg/tb/` (client wrapper + ID/money/code helpers, all pure
  except `client.go`). 16 unit tests cover ID determinism, round-trips,
  bid-model packing.
- Phase 3 — `pkg/billing.Ledger` promoted to an interface; struct renamed
  `MemoryLedger`; `NewLedger` → `NewMemoryLedger`. All call sites updated;
  every existing billing test continues to pass.
- Phase 4 — `pkg/billing/tigerbeetle/` implements the interface. 10 unit
  tests against a fake TB client. Integration test (`go:build
  tigerbeetle_integration`) hits a real TB instance via Tilt port-forward.
- Phase 5 — `cmd/reporting/ledger.go` selects backend at boot. Unreachable
  TB is fatal (no silent fail-open — ledger is source of truth for cost).

**Decisions that diverged from the original sketch:**

- Settle is **3 linked transfers** (post-pending + escrow→publisher +
  escrow→house), not 2. The literal sketch left publisher revenue
  stranded in escrow. Atomic via the TB Linked flag.
- `BalanceFor` on an advertiser account now returns the TB-native
  `debits_posted` (= clearing price). `MemoryLedger.BalanceFor`
  historically double-counted the credit side; callers that relied on
  that quirk will see corrected, lower TotalCredit numbers.

**Carry-over follow-ups:**

- ~~The 8 skipped CPC/CPA/vCPM tests in
  `tests/e2e/billing_models_test.go` remain skipped~~ → **3 of 8 shipped
  (2026-06-04 → 06):** the reporting NATS handlers now call
  `Engine.SettleByTrace` from `handleClick` / `handleConversion` /
  `handleViewabilityFromTracker`. `TestBillingCPCReserveAndSettle`,
  `TestBillingCPAReserveAndSettle`, `TestBillingViewabilityVCPMSettle`
  are active and passing in the e2e suite. The 5 still-skipped tests
  (`TestBillingReservationExpiry`, `TieredRevenueShareTierFlip`,
  `GuaranteedMinimumSubsidy`, `DealTypeFeeModifier`,
  `CurrencyConversion`) are gated on harness helpers (contract-write,
  bulk-auctions, low-TTL cron, exchange_rates seed), not on the
  reporting/ledger path.
- Multi-currency, replicated 3-node TB cluster, and migration of
  in-memory ledger data into TB remain out of scope (see original
  "explicitly out of scope" list below).

**Original plan kept below for the record (decisions resolved):**

## Original Build Plan (kept for reference)

Decided to back the billing ledger with [TigerBeetle](https://tigerbeetle.com/) rather than Postgres. Conceptual fit is exact — TB's pending/posted two-phase transfers map 1:1 onto our reserve/settle pattern, and TB's per-transfer `timeout` field obsoletes the reservation-expiry cron we were planning to build. Trade-off accepted: an extra K8s pod and a slightly less debuggable backing store (no `psql`-style ad-hoc queries), in exchange for the right data model and as a learning exercise.

### Open decisions (resolve before Phase 1 starts)

1. **TB version pin.** Latest 0.16.x, pre-1.0. Pin a specific 0.16 release unless changed.
2. **PVC size.** Default 1Gi for local Tilt; revisit for prod.
3. **House account naming.** `platform:house` (TB account derived from this string). Margin transfers credit it.

### Phases (smaller commits within each)

**Phase 1 — Infrastructure (~30 min).** `k8s/base/tigerbeetle/` StatefulSet single replica + PVC + `tigerbeetle format` init container + `tigerbeetle start` main container on port 3000. Tiltfile entry with port-forward. Verify pod ready + port-forward responds. Commit boundary.

**Phase 2 — Domain bridge `pkg/tb` (~45 min).** Pure functions, no behaviour changes:
- `pkg/tb/client.go` — `NewClient(addresses)` wrapper hiding SDK quirks.
- `pkg/tb/ids.go` — UUID-string → `[16]byte` TB account ID (raw UUID bytes). Per-trace reservation IDs via `idgen.Derive`. Static IDs for house account + USD ledger.
- `pkg/tb/money.go` — `float64 USD ↔ int64 cents`. Same scheme as Redis budget tracker.
- `pkg/tb/codes.go` — enums for the `code` field (1=spend, 2=reservation, 3=settlement, 4=release) + `bid_model → user_data_32` packing.
- Unit tests for ID derivation + money conversion (no TB connection needed).

**Phase 3 — Ledger interface (~45 min).** Pure refactor:
- Promote `pkg/billing.Ledger` (currently a struct) to an interface with the existing public methods (`Record`, `Entries`, `EntriesForTrace`, `EntriesForAccount`, `BalanceFor`, `Summary`, `ReservationByTrace`, `HasSettlement`).
- Existing struct → `MemoryLedger`, still satisfies the interface.
- Update `Engine` constructor + all call sites to take the interface. Existing tests continue to use `MemoryLedger`.
- All existing tests green.

**Phase 4 — TigerBeetle impl `pkg/billing/tigerbeetle` (~1.5-2 hr).** Implements the `Ledger` interface:
- `Record(EntryReservation)` → `CreateTransfers{Flags: Pending, Code: 2, Timeout: 24h, UserData128: traceID, UserData32: bidModelCode}`, debit `advertiser:UUID`, credit `escrow` (shared account).
- `Record(EntrySettlement)` → `CreateTransfers{Flags: PostPendingTransfer, PendingID: …}`. Plus a second non-pending transfer for the margin slice escrow→house.
- `Record(EntrySpend)` (CPM) → advertiser→publisher + advertiser→house, two non-pending transfers.
- `Record(EntryRelease)` → `void_pending_transfer` (or rely on TB timeout — possibly both).
- Account auto-provisioning: `LookupAccounts` → `CreateAccounts` on first sight, with in-process cache.
- Query methods:
  - `BalanceFor(accountID)` → `LookupAccounts`, `debits_posted - credits_posted`.
  - `Summary()` → in-process counter (TB has no GROUP BY).
  - `EntriesForTrace` / `ReservationByTrace` / `HasSettlement` → `GetAccountTransfers` filtered by `user_data_128 = traceID`.
- Integration test against a real TB instance (build-tag-guarded, requires Tilt up).

**Phase 5 — Cutover (~30 min).** New config keys:
- `billing.ledger_backend` = `memory | tigerbeetle` (TierStatic, default `memory` so dev without TB still works).
- `billing.tigerbeetle_addresses` (TierStatic).
- `cmd/reporting/main.go` picks the impl at boot. Re-run CPC + CPA + vCPM e2e tests against TB — should pass identically. Update memory + PLAN.md ledger section.

### Explicitly out of scope this round

- Multi-currency (single USD ledger for now; multi-ledger is small later).
- 3-node replicated TB cluster (prod shape — single replica fine for now).
- Migration of in-memory ledger data into TB (dev ledger is volatile; start fresh).
- Invoice generation. `pkg/billing/invoice.go` already queries the `Ledger` interface; will "just work" once cutover is done but won't be wired in this batch.

### Won't solve

- Invoice generation still requires joining TB transfers with Postgres campaign/publisher metadata in application code. TB doesn't know domain entities.
- Reconciliation between TB ledger and analytics store. Still application code.

### Time estimate

~4 hours of focused work end-to-end. Commit at each phase boundary so we can stop / inspect / roll back if Phase 4's TB API learning curve eats more time than expected.

---

## Next-Up Backlog (2026-06-03)

Concrete, sized work items queued for upcoming sessions. Each entry lists the gap, why it matters, the implementation sketch, and a complexity estimate (S = under 2 hrs, M = half-day, L = 1–2 days, XL = multi-day). Picked up in roughly the order listed unless something jumps the queue.

### Deferred billing / analytics gaps

Carry-over from the analytics-gap audit (2026-06-03). Eight gaps were identified; three (`BudgetDepletedEvent`, video+audio engagement, `ServeNoFillEvent`) shipped that day.

> **Status refresh (2026-07-27):** of the five that were "remaining", **four are
> now DONE** — items 1, 2, 3 shipped since this list was written (the entries
> below are stale) and item 5 shipped today. Only **item 4 (direct-sold CPC/CPA,
> XL + product decision)** is genuinely open.

**1. Privacy opt-out → `OptOutEvent` propagation** — ✅ DONE (`cmd/gateway/privacy.go` records to `opt_out_registry` + publishes `OptOutEvent`; consumers clear per-user state).
- *Gap:* `events.OptOutEvent` + `Publisher.OptOut` are defined but no service ever calls them. Gateway has no opt-out endpoint to receive a user's consent withdrawal.
- *Why it matters:* GDPR / CCPA compliance — when a user opts out, downstream consumers (DSP audience cache, ad server frequency cap, tracker fraud cache) must drop the user's records within minutes, not "eventually."
- *Sketch:* Gateway adds `POST /v1/privacy/opt-out` (consent storage in Postgres `consent_records` table that already exists per memory) + publishes `adtech.privacy.opt_out`. DSP, ad server, tracker subscribe and clear any per-user state on receipt. Existing handlers in `pkg/events/publisher.go` are the right shape.
- *Blocking decision:* opt-out scope per the 3-level model (`docs/PLAN.md` → "User Opt-Out and Data Deletion System"). Just need to commit to which levels the endpoint surfaces.

**2. Tracker fraud-rejection events** — ✅ DONE (`publishRejected` fires from every tracker rejection site — sig/expiry/fraud/dedup in the pixel, view, and media gates; reporting buckets them).
- *Gap:* When tracker drops a fraudulent pixel (HMAC fail, IP/UA blocklist, dedup hit) it logs but doesn't publish. Reporting can count served impressions but has no signal of "we caught fraud spike."
- *Why it matters:* Ops can't alert on fraud volume changes; can't show advertisers "we blocked X% of your fraudulent traffic this period"; can't compute true-cost-per-acquisition (need fraud-adjusted denominator).
- *Sketch:* New subject `adtech.tracker.rejected` + `RejectedEvent{TraceID, EventType, RejectionReason, Timestamp}`. Tracker grows an `events.Publisher` (same pattern pubad uses) and publishes from each rejection site (HMAC validation, fraud realtime middleware, dedup gate). Reporting subscribes + analytics bucket + debug counter + e2e test. Same shape as the analytics-gap closures we just shipped.
- *Implementation note:* Tracker is the highest-RPS service. Fire-and-forget publish is critical; don't block the pixel response on NATS.

**3. Ad server creative-render failures** — ✅ DONE (`adtech.adserver.render_failed` published on resolver-miss / render-error; e2e verifies via the `/debug/render_failures` route).
- *Gap:* When adserver returns 5xx or falls back to default HTML for an unknown creative, no event fires. Silent quality issue.
- *Why it matters:* Operators can't see "creative X is broken in adserver" without log scraping. Real product impact: a broken creative still bills (impression pixel fires) but renders empty.
- *Sketch:* New subject `adtech.adserver.render_failed` + payload with creative_id, reason, trace_id. Ad server's existing `eventPublisher` pattern (we have one — see how tracker is structured). Add publishes at the resolver-miss + render-error sites. Reporting subscribes + counter.

**4. Direct-sold CPC/CPA settle** — 🚫 DECIDED: NOT building it (intentional CPM-only boundary; 2026-07-27)
- *Boundary:* Direct-sold inventory settles **CPM only** (bills at impression). `DirectWinEvent` records serves with `bid_model="direct:<tier>"`; the billing engine has no `direct:cpc`/`direct:cpa` model, so a direct line item can't be sold on a per-click / per-action basis. This is an INTENTIONAL limitation, not a bug or a slippage — CPM is how direct deals actually price.
- *Why we're not building it:* (a) Real-world direct deals are almost always CPM/flat-fee — performance buying (CPC/CPA) goes through the *programmatic* auction, not hand-shook direct deals; there's no prod-parity argument here (unlike Prebid multi-imp). (b) It's **money-path** work that only looks cheap: a `bid_model` column on `publisher_line_items` + config/UI, flipping the direct path from bill→reserve (new `reservation_context` wiring), and — the real cost — **direct economics differ from programmatic** (a direct deal is the publisher's own sale, so the settle revenue split isn't `CalculateRevenue`'s platform-margin model; it needs a direct-specific split). (c) It's gated on a product decision with no default answer: what *is* the price of a direct CPC?
- *When to revisit:* only when a real publisher asks for performance-based direct deals — that ask also answers the pricing-model question. Then: add `publisher_line_items.bid_model TEXT NOT NULL DEFAULT 'cpm'`, stamp it onto the direct serve, reserve on impression (populate `reservation_context`), settle on the tracker click/conversion via `SettleByTrace` (mirrors `cmd/reporting/handleClick`), and define the direct revenue split.

**5. External Prebid bid viewability beacon injection** — ✅ DONE (2026-07-27, commit 953eed9)
- *Resolution:* `writePrebidWinner` now injects a self-contained IntersectionObserver `<script>` (no-SDK port of adtech.js `observeViewability`) into the wrapper alongside the impression pixel, firing the signed `/v1/t/view` on IAB dwell. Also fixed a latent bug: the tracker's view sig validation now excludes the client-measured `dur/pct/area` (appended after signing), so beacons validate under strict signing — which also fixes adtech.js display viewability under strict mode. Unit + e2e covered (`TestPubAdOutboundPrebidViewabilityBeaconFires`, `cmd/tracker/viewsig_test.go`).
- *Original gap:* When external Prebid bid wins (pubad outbound), we render `bid.adm` verbatim. The external bidder's pixels report to their infrastructure — we have no signal that the impression actually rendered, was viewable, or got clicked.
- *Why it matters:* "Zero data slippage" is the platform's stated goal but this path slips. Without our own beacon, fill-rate analytics overcounts (we'd record the Prebid win regardless of whether the creative actually rendered).
- *Sketch:* Pubad's `writePrebidWinner` injects our `<img src="${VIEWABILITY_URL}">` and impression pixel into the bid's `adm` HTML before serving. Tracker beacons fire alongside the external bidder's. Reporting attributes views to the Prebid endpoint via a new `prebid_endpoint` column on `view_events`.
- *Subtle:* If the external bid's `adm` is a script or iframe (common), simple HTML injection may not work. May need an outer wrapper div with our pixel + the bid contents inside.

### Video / CTV measurement

**6. Video viewability (IAB 50% / 2s)** (L — its own project)
- *Gap:* The video/audio path emits the full VAST tracking set (impression, quartile funnel → VCR, mute/pause/resume/skip/fullscreen, click, error), but there is **no video viewability** metric. Video viewability (IAB/MRC standard: ≥50% of the ad's pixels on-screen for ≥2 continuous seconds) is a *distinct* measurement from completion — the quartiles tell you how much was watched, not whether it was on-screen. Display has viewability (`/v1/t/view`); video does not.
- *Why it matters:* Video viewability is a headline CTV/OLV buying metric (advertisers pay on viewable impressions, vCPM). We already settle vCPM for display views; video vCPM can't be honoured without a video-viewable signal. Also "zero data slippage": we report VCR but stay silent on viewability, so a viewability-bought video campaign has a blind spot.
- *Why it's not just another VAST event:* Viewability isn't in the VAST tracking vocabulary — the industry measures it via the **Open Measurement SDK (OMID)**, which the player loads and which observes the ad's actual on-screen geometry. VAST only carries an `<AdVerifications>` pointer to the OM verification resource (we already emit that element for OMID when `omid_verification_url` is set).
- *Sketch — two options:*
  1. **OMID integration (industry-standard, but heavy + reintroduces a Google/IAB SDK):** ship the OM Web SDK, register the ad session, and let it fire the viewable beacon. Faithful, but it's another third-party SDK on the client — cuts against the "everything local / no external player" direction we took by dropping IMA.
  2. **Self-measured (fits our local-first stance):** in our own player (`minimal.html playNextVideoAd`, and in `adtech.js` for a shipped video player), use an `IntersectionObserver` on the `<video>` element to track the % of pixels in the viewport, start a 2s timer when it crosses 50%, and fire a signed `/v1/t/view` (or a new `/v1/t/video?event=viewable`) beacon once the dwell completes — exactly how the display viewability path works, applied to the video element. No external SDK. Tracker already computes the server-authoritative IAB verdict for display; extend it to a `video` channel viewable event, persist `ViewEvent{Channel:"video", …}`, and let `handleViewabilityFromTracker` settle video vCPM (the vCPM settle machinery already exists).
- *Decision needed:* OMID (standard, external SDK) vs. self-measured IntersectionObserver (local, ours). Given we just removed IMA to stay Google-free, the self-measured route is the more consistent choice; OMID only if a real buyer requires certified OM measurement. Note: SSAI/CTV viewability is harder still (server-side, no client geometry) — punt until client-side video viewability lands.

### Other tracks queued behind Prebid

Items the user surfaced as "we'll handle them next" while we focused on Prebid.

**6. ~~Seed-vs-schema-default bug~~** ✅ DONE
- *Resolution:* `config.WithSeedDefaults(overrides)` Setup option ships
  the per-pod override map at boot. `cmd/dsp/main.go` calls
  `dspSeedOverrides(profileName)` which reads the YAML profile and
  hands the `noise_pct`/`no_bid_rate` per-profile values to
  `config.Setup`. Registry's seed step uses the override when present;
  `migrateSeedDefaults` (in `pkg/config/setup.go`) updates rows that
  still hold the original schema default so existing deployments pick
  up the fix without manual SQL. Verified by
  `TestSeedDefaults_*` e2e tests (competitor1 noise=30, competitor2
  noise=40, internal stays 0).

**7. Real auth on management endpoints** (M)
- *Gap:* `cmd/dsp/management.go` (campaign CRUD) and `cmd/ssp/management.go` (placement CRUD) have no auth. Anyone on the network can create / mutate / delete campaigns and placements. Acceptable in dev; unacceptable in any deployment past `tilt up`.
- *Why it matters:* Memory flags it as TODO in source: "Production auth middleware for DSP campaign CRUD + SSP placement CRUD — both noted as TODO in source; must not be exposed publicly until that lands." We have `pkg/middleware/Auth` and `pkg/middleware/RequirePermission` for the API proxy paths — these CRUD endpoints aren't wired.
- *Sketch:* Wrap the relevant handlers with `middleware.Auth(signingKey)` + `middleware.RequirePermission("campaigns:write" / "placements:write")`. Add the permissions to the role definitions. The gateway already issues JWTs — internal services just need to validate them. JWT signing key flows via config (`gateway.jwt_signing_key`). E2E test: assert unauth'd PATCH returns 401, authed advertiser-token can PATCH own campaign, can't PATCH other tenant's.
- *Side cleanup:* the `External DSP Partners onboarding plan` (per memory) is gated on this work — partners need to authenticate before being trusted to land bids.

**8. Publisher-adserver: preferred-tier + competitive exclusion** (L)
- *Gap:* Original publisher-adserver design (in PLAN.md) listed four tiers — sponsorship, guaranteed, **preferred**, house — but the starting scope shipped only the first, second, and fourth. Preferred deals (publisher-side: rate floor without volume commitment) aren't implemented. Competitive exclusion (Coke creative blocked while Pepsi line item is serving on the same page view) also deferred.
- *Why it matters:* Preferred is a real publisher product (publishers often have 5-10 preferred buyers before opening up open-market). Competitive exclusion is table-stakes for premium publishers — luxury brand advertisers won't book if competitors can run adjacent.
- *Sketch:*
   - *Preferred:* Add `preferred` to the priority_tier enum + arbitration ladder. Behaviour: "rate floor without preempt" — sets `effective_floor = max(placement_floor, preferred_cpm)` for the programmatic auction, doesn't preempt. Same shape as deals.Preferred at the exchange layer; pubad has its own equivalent. ~half-day.
   - *Competitive exclusion:* Add IAB-category exclusion list to `publisher_line_items` + a Redis page-view-scoped lock. When a sponsorship for IAB-22 (Personal Finance) is served on `page_view_id=X`, set `pubad:exclusion:{X}:IAB-22` for 5 minutes. Subsequent serves on the same page_view fetch the lock and exclude matching categories from arbitration. ~full day including the page-view-id propagation work.
- *Open question for preferred:* how do we surface the negotiated rate to the SSP so the auction enforces it? Either pubad adds a `bidfloor` override on the OpenRTB request before sending to SSP, or pubad evaluates the response and overrides the price. Mirrors what `pkg/deals` already does at the exchange layer for advertiser-side preferred deals.

### New (2026-06-06): Full event-pathway sweep

**9. End-to-end event-pathway audit** (L) — `zero data slippage` audit
- *Gap:* We've been closing event-pathway holes piecemeal — opt-out
  events defined but not published; tracker rejection events defined
  but not published; ad server render-fail events not modelled; some
  Prebid outcomes not beaconed; etc. (See items #1, #2, #3, #5 above
  for individual holes already catalogued.) What we don't have is a
  single audit pass that walks every event source → bus subject →
  consumer(s) → analytics + ledger landings and confirms each step
  is wired, logged, and observable.
- *Why it matters:* The platform's stated goal is "zero data
  slippage" but the only way we currently verify that is the e2e
  suite + memory. With ~30 NATS subjects across 9 services + 3
  storage sinks (Postgres, analytics store, billing ledger), a
  spot-check approach will keep missing things. CI catches missing
  publishes only when a test exists for that specific signal.
- *Sketch:*
   - *(a) Inventory.* Generate a table of (publishing site, subject,
     payload type, consumer service(s), analytics bucket, billing
     landing, slog message). Source from `pkg/events/subjects.go` +
     `pkg/events/payloads.go` + `grep -rn 'bus.Publish\|publisher.Publish'`
     across `cmd/`. Note publishers that don't have consumers and
     consumers that subscribe to undefined subjects.
   - *(b) Wiring check.* For every NATS subject we publish, assert
     downstream landings: (i) at least one e2e test fires the event
     and asserts on a queryable downstream signal (analytics row,
     billing ledger entry, debug counter, log line), (ii) the consumer
     ACKs on success and NAKs on transient error (idempotent retry),
     (iii) the payload is versioned (`schema_version` field present).
   - *(c) Logging check.* Every event source has a structured slog
     line at the publish site, with `trace_id`, `subject`, and a
     business-relevant identifier (campaign_id, placement_id,
     account_id). Every consumer logs receipt with the same `trace_id`
     so a Loki query can stitch the flow end-to-end.
   - *(d) Observability dashboard.* Grafana panel per subject with
     publish-rate + consume-rate + lag percentile. One scrape config
     covers all `/metrics` endpoints. Trace explorer already pivots
     by `trace_id` (per memory) — this dashboard pivots by subject.
   - *(e) Plug the gaps.* For every missing publish, missing consumer,
     missing test, missing log, or missing dashboard panel: fix in a
     small commit with the specific gap referenced.
- *Decision needed:* whether to add subject inventory to the build
  (a `go:generate` that walks the codebase) or maintain it manually
  alongside `pkg/events/subjects.go`. Auto-generation catches drift
  but is brittle; manual stays in sync with intent but rots.
- *Subsumes:* items #1, #2, #3, #5 from this backlog become bullets
  inside #9's "plug the gaps" step. Track them there once the sweep
  starts.

### Order

Suggested order if no other priority intervenes (updated 2026-06-06,
post pod-migration + e2e green; #6 already shipped):
1. **#9 Event-pathway sweep** — frame items #1, #2, #3, #5 under one
   audit so we close all the "zero data slippage" gaps in one consistent
   pass instead of piecemeal. Inventory first, then fix per-gap.
2. **#7 Auth on CRUD** — blocks any non-dev deployment. Must land before
   we can usefully test external DSP Partners.
3. **#8 Pubad preferred + exclusion** — biggest in this list. Schedule
   its own session.
4. **#4 Direct CPC/CPA** — deferred until a real publisher asks; no
   work item, just a documented limitation.

### Smaller-than-backlog items also worth picking up

These came out of the pod-migration session and don't merit a full
backlog entry, but each is a couple-hour fix:

- **Chaos test helpers** — 4 skipped tests
  (`TestChaos{Redis,NATS,Postgres,Minio}Down`) want a
  `harness.ChaosKill{Service}` that runs `kubectl delete pod -l app=X`.
  Together with a `WaitForReady` retry, this unsticks all 4.
- **Migration step harness helper** — `cmd/migrate` only does
  up-to-latest. A `goose up-by-one` / `goose down-by-one` mode would
  unstick `TestMigrationForwardPreservesData` /
  `TestMigrationRollbackSafety`.
- **Jaeger client wrapper** — would unstick
  `TestTracePropagatesSSPToExchangeToDSP`. Just a `GET
  /api/traces?traceID=…` against the in-cluster Jaeger.
- **Bulk-auctions harness helper** — needed for tiered revshare /
  guaranteed minimum tests. Just a loop of `RunAuction + FireImpression`
  with progress logging.
- **RLS test postgres role** — `TestRLSIsolation` skips because the
  dev `adtech` role is BYPASSRLS superuser. Add an app-tier
  non-superuser role for tests that need to exercise the policies.

---

## Open Architecture Decisions

Capture for choices that don't have a single right answer and shouldn't be silently picked in code. Each item lists the question, the live options, and the work that's blocked.

### vCPM Settlement Model

**Status: DECIDED 2026-06-01 for sub-decisions 1, 3, 4. Sub-decision 2 (reservation timeout) still open — waiting on the expiry cron.**

**Question:** When a `vcpm` bid wins an auction and the impression renders, how and when does spend hit the ledger?

**Context:** Viewability data already flows end-to-end as of 2026-06-01 — the tracker computes the server-authoritative IAB verdict and persists `ViewEvent{IABViewable, …}` to analytics. What's pending is the *billing* settlement: for `bid_model = vcpm` line items, the impression event must not bill immediately the way CPM does. It has to hold and only commit on a verified view.

**Sub-decisions:**

1. **Spend-accrual pattern** — pick one:
   - *Reserve at impression, settle at view.* Auction win → reserve budget. View event arrives → reservation becomes spend. Timeout with no view → release. (Most common in real platforms; matches the existing CPC/CPA reserve-settle plumbing in `pkg/billing.Engine.ProcessEvent`.)
   - *Bill only on view.* No reservation, no holding pattern. Impression is logged but doesn't touch the budget. Cleaner ledger math; messier pacing because a campaign can over-deliver before any spend is recorded.
   - *Bill at impression, claw back on no-view.* Treat as CPM, refund if view never lands. Clean from a pacing view; nightmare from a reconciliation view. Not recommended.

2. **Reservation timeout** — how long do we hold a reservation before releasing? Industry convention is 24–48h; would become `billing.vcpm_view_window` config key.

3. **Default for `IABViewable = false`** — view event arrived but below threshold:
   - Release reservation (advertiser only pays for genuinely viewable inventory).
   - Bill at impression rate (publisher gets paid for delivery).
   Contract-dependent in real platforms; pick a platform default with per-IO override later.

4. **Rate source** — use the auction's `clearing_price` (what the DSP bid `vcpm` at), or a separate `vcpm_rate` field on the line item.

**Chosen defaults (2026-06-01):**
1. **DONE.** Reserve at impression, settle at view. `cmd/reporting/main.go:handleView` calls `Engine.SettleByTrace(ctx, traceID, "viewable")` when `IABViewable=true`. Same reserve/settle plumbing as CPC/CPA.
2. **OPEN.** 24h reservation timeout. Pending the expiry cron — until that lands, reservations from non-viewable or never-viewed impressions accumulate in the in-memory ledger until reporting restarts. Tracked by the skipped `TestBillingReservationExpiry`.
3. **DONE.** `IABViewable=false` → no settlement. Platform default contract semantic is "viewable or nothing"; we do not downgrade to CPM rate.
4. **DONE.** Rate = `clearing_price` from the reservation row (no separate `vcpm_rate` field on the line item).

**Status:** Layer 1 (contract data) landed earlier — `line_items.viewability_target_pct` + `deals.viewability_target_pct` in migration 025, plumbed through `models.Campaign` + `models.Deal` + the warm-cache loaders. Layer 2 (settlement) is wired for the chosen defaults; only sub-decision 2 remains open. `TestBillingViewabilityVCPMSettle` un-skipped and asserts both the viewable-settles and non-viewable-doesn't-settle paths.

---

### Prebid Server Integration

**Status: DESIGN — committed in principle 2026-06-02, scope and integration shape still open.**

**Question:** Prebid is the open-source de-facto standard for header bidding — the in-page (or server-side) auction that runs across many SSPs/exchanges in parallel before the publisher's ad server is called. We want our platform to participate. What shape does that integration take?

**Background:**
- `Prebid.js` runs in the browser; the publisher drops it on their page and configures bidder adapters (~300+ adapters exist for major SSPs/exchanges).
- `Prebid Server` (Go and Java reference impls) is the server-side version — reduces browser load, hides bidder list from competitors, mandatory for AMP and most CTV.
- Prebid is governed by IAB Tech Lab. Adapter spec is well-defined; bidders implement an HTTP endpoint that takes a Prebid `BidRequest` (OpenRTB-shaped with some extensions) and returns a `BidResponse`.

**Sub-decisions:**

1. **What role do we play?** Three plausible positions, not mutually exclusive:
   - *(a) Bidder.* Other publishers' Prebid setups call us as one demand source among many. We expose a Prebid-compatible HTTP endpoint (OpenRTB 2.5/2.6 with Prebid extensions). Our exchange becomes the receiver of Prebid traffic. **Lowest cost, highest reach.**
   - *(b) Host a Prebid Server.* Run our own Prebid Server instance for publishers who use us as their primary SSP. Acts as a single server-side header-bidding endpoint that fans out to many bidders (us + competitors). Closer to what a prior ad-tech employer/PubMatic do as "managed Prebid."
   - *(c) Ship a Prebid.js adapter.* Write and contribute the canonical `ourPlatformBidAdapter.js` to the Prebid.js repo so publishers using stock Prebid.js can add us via config. Required if anyone is going to discover us via Prebid's prebid.org/dev-docs bidder list.
   
   (a) is table stakes; (b) is a product expansion; (c) is a distribution play. Most likely all three eventually, but pick a start.

2. **Where does the bidder endpoint live?** Either:
   - A new path on the existing `cmd/exchange` (e.g. `/v1/prebid/openrtb2/auction`) — Prebid Server speaks OpenRTB 2.x, which is close to what the exchange already handles. Translation layer maps Prebid extensions → internal bid request shape.
   - A new `cmd/prebid-adapter` service that translates Prebid requests → internal SSP-style requests and forwards. Keeps the exchange clean of Prebid-specific extension handling.
   
   The exchange already accepts OpenRTB-shaped bid requests; the cheaper path is a new handler on it.

3. **Server-side vs client-side bidder identity.** When we're a bidder, do we want to be called from Prebid.js (browser) or Prebid Server (publisher backend)? Server-side is faster (no browser RTT), better for CTV/AMP, and gives publishers a unified bidder list — but it requires our `seller.json` and `ads.txt` records to be set up properly. Probably support both; the wire format is nearly identical.

4. **User sync / cookie matching.** Prebid bidders typically expose a `/setuid` endpoint that lets the Prebid Server set a cookie mapping the publisher-side user ID to our internal ID. Without it, we can't recognise repeat users across Prebid auctions. Cookieless future complicates this — likely we lean on the identity-graph work already underway (`pkg/identity/`) rather than building a separate cookie sync.

5. **Prebid.js adapter (option c) shape.** If we contribute an adapter, what's the minimum viable spec?
   - Bidder code (the unique short identifier in Prebid's registry — e.g. `appnexus`, `rubicon`).
   - Parameters publishers configure on each ad unit (placement ID, optional floor, optional deal IDs).
   - Request builder + response parser (JS code in the Prebid.js repo).
   - Adapter docs + sample config (PRs to prebid.org documentation).

**Recommended starting scope:**
- Build (1a) + (2 = new handler on exchange) + (4 = setuid endpoint using existing identity store). Defer (1b) host-your-own Prebid Server and (1c) Prebid.js adapter until we have any external user.
- Wire path: external Prebid Server → `POST exchange/v1/prebid/openrtb2/auction` → translation to internal `openrtb.BidRequest` → fan-out to DSPs (reusing the smart router) → bid response in Prebid format.

**Decisions locked 2026-06-02:**
- **Bidder code:** `adtechmono`. Used as the unique short identifier in any future Prebid registry submission and in our public Prebid-compatible endpoint docs.
- **Bidfloor handling:** Effective floor = `max(prebid_bidfloor, our_placement_floor)`. Honours both publisher-side dynamic-floor logic (which Prebid Server has already applied upstream) and our own constraints (deal floors, fraud minimums). Matches AppNexus / PubMatic / Prebid Server reference behaviour.
- **Deal IDs:** Pass through opaquely. The Prebid handler accepts `deals[]`, logs each ID, but does not attempt to match against internal `pkg/deals`. Registry table is an additive future change if we onboard a real partner whose deals should trigger our PG/Preferred/PMP priority logic.

**Blocked work:** Nothing — additive feature. Touches `cmd/exchange`, adds Prebid translation layer, possibly new `cmd/prebid-adapter` if we pick option (b) later.

**Known gap — multi-imp requests (G1 in the competitive auction suite, currently SKIPPED):**

Prebid commonly sends multi-impression requests — a single OpenRTB body can carry 3+ imps representing several ad slots loaded together on one page (header bidding's "auction the whole page at once" model). Today `cmd/exchange/main.go` `auctionHandler` reads `bidReq.Imp[0]` and runs one auction for that imp; any additional imps in the same request are ignored. The Prebid bidder endpoint inherits this limitation because it dispatches through `auctionHandler` unchanged.

What landing this needs:
1. **Auction loop per imp.** `auctionHandler` rewritten to iterate `bidReq.Imp`, running the deal-eval + auction + winner-pick path per imp. Today's single-imp shortcuts in floor lookup (`bidReq.Imp[0].BidFloor`) and placement extraction need a per-imp version.
2. **Response aggregation.** `openrtb.BidResponse.SeatBid` should carry one `Bid` per winning imp (currently emits one). Per-imp winners may come from different DSPs — the response groups by seat.
3. **Per-imp NATS events.** `AuctionWinEvent` is currently one event per request; with multi-imp it becomes N events (one per winning imp). Reporting consumer + billing ledger key on `imp_id` to distinguish.
4. **Per-imp win/loss notifications.** Fan-out today notifies each DSP once with the auction's clearing price; multi-imp means each DSP gets notified per imp it bid on (won/lost). DSP-side handlers don't need changes — they already accept per-bid GETs.
5. **Per-imp deal evaluation.** Deal matcher already keys on (publisher, placement, advertiser); each imp's matches are evaluated independently. PG preempt for imp A doesn't affect imp B.
6. **Span/log model.** The current `exchange.auction` span carries one set of `winner_dsp` / `clearing_price` attributes. Multi-imp needs either child spans per imp (cleaner, more traffic) or array-valued attributes (compact, harder to query in Jaeger). Recommend child spans.

> **Scope correction (2026-07-27, after reading both hot paths — see
> [`docs/PREBID_MULTI_IMP.md`](PREBID_MULTI_IMP.md) for the full design note):**
> the "~1 day, exchange-only" estimate above is TOO LOW, and item 4's "DSP-side
> handlers don't need changes" is wrong for the BID handler. Two corrections:
> - **The DSP bid handler also hardcodes `Imp[0]`** (`cmd/dsp/main.go` ~896–1141:
>   floor/format/dimensions/response `ImpID`), so it must loop over imps and bid
>   per imp too. And `auction.Bid` has no `ImpID`, so bids can't be grouped per
>   imp without a core-type change.
> - **The real blocker is the `trace_id`/billing invariant, not the handlers.**
>   `AuctionWinEvent` is the single source of truth for cost, keyed on `trace_id`,
>   and the tracker + billing dedup on it. N wins in one request need N distinct
>   `trace_id`s threaded through render → track → bill, or two rendered ads
>   collapse to one billed cost (silent under-billing — the exact "data slippage"
>   the platform exists to prevent). That makes this a **pipeline-wide** change
>   (exchange, DSP, SSP, ad server, tracker, billing + the trace-id-per-imp
>   model), whose FIRST deliverable is the per-imp trace-id design.

Why not done now: every existing test sends single-imp requests, so the gap doesn't break anything today (a multi-imp request just fills the first slot — a scoping limitation, not a mis-billing bug). Real external Prebid integrations would surface it immediately. Build before any first external Prebid publisher onboards, as its own focused effort (design the trace-id model first). Tracked by skipped `TestCompetitiveG1_PrebidMultiImpRequestPerImpAuction`; full design in [`docs/PREBID_MULTI_IMP.md`](PREBID_MULTI_IMP.md).

---

### Publisher-Side Ad Server ("GAM-shaped" features)

**Status: DESIGN — confirmed direction 2026-06-02, scope and arbitration semantics open.**

**Question:** Our current stack is end-to-end *programmatic* — SSP → exchange → DSPs → ad server. Real publishers also have **direct-sold campaigns** (sales team signs Nike for 1M impressions at $50 CPM), **house ads** (promote own products in unsold inventory), and **passback chains** (fall back to another network when nothing fills). Today, programmatic is the *only* demand source the slot ever sees. A publisher ad server arbitrates across all of these — direct-sold, house, programmatic, passback — and picks per impression.

**Background:** Industry term is "ad serving / inventory management" — DoubleClick for Publishers / Google Ad Manager (GAM) is the dominant player; alternatives include Kevel, AdButler, Smart AdServer, Equativ, Xandr Monetize. GAM's "Dynamic Allocation" is the canonical arbitration model that header bidding was specifically designed to escape from.

**The arbitration ladder (per impression):**
1. **Sponsorships / roadblocks** — "Nike owns 100% of homepage on June 5th." If active and eligible, short-circuits everything.
2. **Guaranteed line items** behind pace — contractually obligated delivery; serve even if programmatic would pay more.
3. **Non-guaranteed (preferred) line items** whose negotiated rate beats best programmatic bid.
4. **Programmatic** — call into our SSP → exchange → DSP chain. (This is where our existing stack lives today.)
5. **House ads** — promote own products, free fill for inventory that didn't clear floor.
6. **Passback** — emit another ad network's tag if nothing above filled.

The product *is* the arbitration. Direct-sold line items are obvious; the harder parts are pacing, competitive exclusion, and forecasting.

**Sub-decisions:**

1. **Service shape.** Two options:
   - *(a) New `cmd/publisher-adserver`* that sits in front of the SSP. Browser → publisher-adserver → arbitration → (direct win OR call SSP). Cleanest separation; SSP stays purely a programmatic-demand-source surface.
   - *(b) Fold arbitration into SSP.* SSP grows a "direct-sold check" pass before triggering the auction. Simpler to ship but conflates two distinct products (selling inventory vs. arbitrating inventory).
   
   Option (a) matches real-world architecture (GAM and SSPs are separate products at every publisher I've seen). Recommended.

2. **Direct line item data model.** Need a new entity, distinct from `line_items` (which today holds advertiser-side line items). Proposed `publisher_line_items` table:
   - Owner: publisher (`publisher_id`), not advertiser.
   - Demand source: the brand/agency they sold to (text field; not necessarily an account in our system since direct deals can involve brands that never log into our DSP).
   - Targeting: publisher-side targeting (which placements, which page categories, which geos).
   - Delivery commitment: `impressions_committed`, `delivery_start`, `delivery_end`.
   - Pricing: fixed CPM (most direct deals are CPM), with optional CPC/CPA variants.
   - Priority tier: `sponsorship | guaranteed | preferred | house`.
   - Creatives: linked to existing `creatives` table (publishers upload direct-sold creatives just like advertisers do).
   - Pacing target: usually "even" (deliver linearly across the flight) or "asap" (front-load).

3. **Pacing controller.** Per-impression decision: "should I serve this direct line item right now?"
   - Compute `expected_impressions_so_far = total_committed * (now - start) / (end - start)`.
   - Compute `actual_impressions_so_far` from the ledger.
   - If actual < expected → behind pace, serve.
   - If actual > expected by significant margin → ahead of pace, defer to programmatic this impression.
   - More sophisticated: probabilistic pacing (serve with probability `p` calibrated to hit target by end-of-flight). Similar in shape to `pkg/dsp/budget` pacing on the DSP side — likely a new `pkg/pubad/pacing` package.

4. **Arbitration vs programmatic.** Critical correctness question: when does direct-sold win over programmatic?
   - *Sponsorships* always win when active.
   - *Guaranteed behind pace* always wins (contract trumps revenue).
   - *Guaranteed on-pace* — option: still wins (publisher prefers locked revenue over auction uncertainty), or go to auction with effective floor = guaranteed CPM (publisher takes whichever pays more, but risks under-delivering the guarantee).
   - *Preferred / non-guaranteed* — go to auction with effective floor = preferred CPM. If programmatic clears, take programmatic; else serve preferred.
   
   GAM's default is "guaranteed always wins"; "guaranteed on-pace goes to auction" is a more recent product called "first look" / "open bidding." Pick a default; expose per-line-item config.

5. **Competitive exclusion.** If a Coca-Cola direct campaign is running on this page view, exclude all Pepsi creatives from any source (direct OR programmatic) for that page view. Requires:
   - Industry-category taxonomy on creatives (we already have IAB categories per `pkg/constants`).
   - Per-line-item "competitive exclusion list" config.
   - Page-view-scoped state (which categories are already locked on this page view). Lives in a short-lived Redis key keyed by page-view ID.

6. **Forecasting.** Sales team asks: "can I commit to selling 5M impressions on sports content in July?" Need an inventory forecast. Stats job over historical SSP request volume + segmentation by targeting attributes. Plausibly built later — Phase 11+ — but worth flagging now because it shapes the data we want to retain.

7. **Passback / fallback chains.** When nothing fills, emit another ad network's tag. Typically a sequence of tags tried in order (third-party JS / iframes / VAST URLs). Adds passback support to the ad server's response path.

8. **UI surface.** Direct-sold inventory needs a new admin UI:
   - Publisher trafficking view (create/manage `publisher_line_items`, upload creatives, set targeting, view pacing).
   - Pacing dashboards (per-line-item delivery progress, projected end-of-flight delivery, behind-pace alerts).
   - Forecasting UI (later).
   
   Plausibly a new section under the existing publisher console (whatever surface ends up holding `cmd/ssp/management.go`'s placement CRUD).

**Where this slots into the request flow:**

Full ASCII flow diagram covering all three entry points (publisher visitor, external Prebid Server, browser-fired pixels) plus the post-serve event chain lives at `docs/diagrams/request-flow.txt`. Quick summary:

```
Browser → Publisher Ad Server (arbitration)
              ├─ sponsorship/guaranteed wins → Ad Server (render direct creative)
              ├─ house ad → Ad Server (render house creative)
              ├─ programmatic → SSP → Exchange → DSPs → winner → Ad Server (render)
              └─ passback → emit fallback tag
```

`cmd/ssp` is unchanged structurally; it just becomes one demand source the publisher ad server calls. `cmd/exchange` and downstream are untouched.

**Recommended starting scope (sub-phase 1):**
- New `cmd/publisher-adserver` service.
- `publisher_line_items` table + RLS policy + warm cache.
- Priority tiers: sponsorship + guaranteed + house (skip preferred initially).
- Basic even-pacing controller.
- Arbitration: sponsorship-always-wins, guaranteed-always-wins, fall through to programmatic, house-on-no-fill.
- Defer: competitive exclusion, forecasting, passback, "first look" guaranteed-goes-to-auction.

**Decisions locked 2026-06-02:**
- **Service name:** `cmd/publisher-adserver`. Verbose but unambiguous; clear contrast with `cmd/adserver` (creative renderer) for newcomers reading the cmd/ directory.
- **Ingress:** Behind the existing gateway. Browser → `cmd/gateway` → `cmd/publisher-adserver`. Consistent with how every other browser-facing surface is fronted (CORS, rate limiting, observability all in one place). Adds ~1ms hop; acceptable.
- **Pacing controller:** New `pkg/publisheradserver/pacing` package. DSP budget = stay under a ceiling; publisher pacing = hit a delivery commitment. Different problem semantics; sharing code would obscure the asymmetry. If Redis counter helpers end up duplicated, lift them into a shared `pkg/cache` util later.
- **Pub sim wiring:** Once `cmd/publisher-adserver` exists, the pub sim switches to calling it instead of the SSP directly. Most accurate model of a real publisher (which never calls an SSP directly — always its own ad server). The trace timeline becomes a full publisher-adserver → SSP → exchange → DSP chain.
- **PubAd ↔ Prebid order:** `cmd/publisher-adserver` is in front; Prebid is one demand source it can call. Browser → publisher-adserver → arbitrate direct-sold first → if no direct win, call Prebid → winning Prebid bid is the programmatic candidate (which may then lose to a guaranteed-but-not-yet-evaluated tier, depending on arbitration order). Matches the real GAM+Prebid flow. The publisher is the inventory owner; Prebid is a sourcing mechanism it opts into.

**Integration note (PubAd + Prebid + existing stack):**

Once both land, the publisher-adserver fans out demand sources in a defined order per impression:
1. Direct-sold tiers (sponsorship → guaranteed behind pace → guaranteed on pace if "always wins" set).
2. Programmatic auction — our own SSP/exchange chain.
3. Prebid Server — fan out to external Prebid bidders for additional demand on the same impression.
4. House ads.
5. Passback.

Programmatic and Prebid both produce a winning bid that competes against any "preferred" tier direct-sold line items (which have a floor but no commitment). House and passback only fire if nothing above filled.

This puts Prebid in the same "external programmatic demand" bucket as our own exchange chain — which is correct, because from the publisher's perspective our exchange and a third-party Prebid bidder are both just sources of programmatic dollars competing for inventory the publisher owns.

**Blocked work:** This is purely additive. Touches new package `pkg/publisheradserver`, new service `cmd/publisher-adserver`, new migration for `publisher_line_items` + related tables. Probably 2–3 weeks of build once design is locked.

---

### Auth Infrastructure (secrets-as-data, warm-cached)

**Status: DESIGN — direction agreed 2026-06-03, implementation queued.**

**Question:** Where do auth credentials live and how do they propagate? Today `gateway.jwt_signing_key` is a config-as-static value baked in at boot; `cmd/dsp/management.go` and `cmd/ssp/management.go` skip auth entirely; external Prebid traffic to `cmd/exchange` has no per-partner authentication; service-to-service calls are pure network trust. Multiple auth gaps with no unified mechanism.

**The shape we're committing to:** **auth-as-data**, not auth-as-config. Secret material lives in Postgres tables, loaded by services through the same warm-cache primitive (`pkg/cache/warm`) that already serves campaigns / placements / creatives / deals. A high-level operator manages all secrets through the existing config-manager UI (new "Secrets" tab); rotations propagate to every service via NATS within milliseconds; brief outages during rotation are acceptable.

**Why this shape (vs config-as-static):**
- Rotation without redeploy. Operator clicks "Rotate" → broadcast → every pod picks up new value within NATS round-trip time.
- Audit trail in the same audit_log table all other config changes use.
- No new infrastructure: same warm-cache + NATS-invalidate mechanism we already trust for hot-path data.
- High-level operator can manage every auth surface (JWT signing keys, HMAC secrets, partner shared secrets, API keys) from one UI without touching env vars / K8s Secrets / pod restarts.

**Decisions locked 2026-06-03:**

- **Two tables, not one:** generic `secrets` for opaque platform-managed material (JWT signing, HMAC, partner shared, service-to-service shared) — managed via the operator UI. Existing `api_keys` table stays separate for per-user-issued keys (advertiser / publisher self-service via the dashboard; different lifecycle, different UI surface).
- **Warm-cache pattern matches existing platform code:** `pkg/cache/warm.Cache[Secret]` snapshot-backed, atomic.Pointer reads, 30s poll, NATS invalidate on `adtech.cache.invalidate.secrets`. Per-request reads are microsecond map lookups — zero Postgres round-trips on the hot path.
- **Per-service filtering:** each service loads only the secrets it needs (`WHERE purpose IN (...) AND (owner = 'platform' OR owner = $service)`). Exchange doesn't load SSP's JWT signing key; gateway doesn't load tracker's HMAC keys. Smaller blast radius if any one service leaks its cache.
- **Continue-with-empty-cache on boot:** if Postgres is unreachable at boot, the cache logs a warn and proceeds empty. `/readyz` returns 503 until first successful load. The poll loop retries every 30s, so a Postgres blip during deploy doesn't restart-loop the platform. Matches existing pattern in every other warm-cache caller (campaigns, deals, placements all use this).
- **Rotation grace window:** when a secret is rotated, the old row goes `status=rotating` for a configurable window (default 5 minutes); during that window, validators accept BOTH old and new. After grace expires, old goes `status=revoked` and only new validates. JWT-style secrets validate against the current snapshot's active + rotating values; HMAC-style secrets check against both. Brief outages during the transition are acceptable per the agreed semantics — operator just re-rotates if propagation was slower than expected.

**Schema sketch (subject to refinement at implementation time):**

```sql
CREATE TABLE secrets (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name        TEXT NOT NULL,                 -- "jwt-gateway-signing", "prebid-partner-acme"
  value       TEXT NOT NULL,                 -- the secret (encryption-at-rest TBD)
  purpose     TEXT NOT NULL CHECK (purpose IN (
                'jwt_signing', 'hmac_tracker',
                'partner_shared', 'service_s2s'
              )),
  owner       TEXT NOT NULL DEFAULT 'platform', -- "platform" or service name
  status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN (
                'active', 'rotating', 'revoked'
              )),
  rotated_at  TIMESTAMPTZ,                   -- when status went rotating
  revokes_at  TIMESTAMPTZ,                   -- when rotating→revoked
  expires_at  TIMESTAMPTZ,                   -- optional natural expiry
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_secrets_purpose_owner ON secrets (purpose, owner) WHERE status != 'revoked';
CREATE INDEX idx_secrets_name ON secrets (name) WHERE status != 'revoked';
```

**Open questions to resolve before Phase 1:**

1. **Encryption at rest.** Three options:
   - *(a)* Plain `TEXT` column, rely on Postgres access controls + transport encryption. Simplest for local dev; acceptable for platforms where the DB itself is the security boundary.
   - *(b)* `pgcrypto` column-level encryption with a per-platform master key in a K8s Secret. Adds DB CPU cost on every read; warm cache reduces this to once-per-poll-cycle.
   - *(c)* External secret manager (HashiCorp Vault sidecar, AWS Secrets Manager). Highest security; requires infrastructure we don't currently run.
   - **Recommend:** defer to per-overlay decision. Local dev uses (a); staging/prod overlay flips to (b) via a config flag on PostgresSource. (c) is a future migration once we have a real customer with the requirement.

2. **Bootstrap root credential.** Where does the very first auth come from?
   - Env var `PLATFORM_ROOT_PASSWORD` set at deploy time, used only to create the first operator account via a one-shot `/v1/auth/bootstrap` endpoint that disables itself after first use.
   - Operator creates other operators via the UI thereafter.
   - "Break glass" rotation: env var change + pod restart re-enables the bootstrap endpoint.

3. **Rotation grace window default.** 5 minutes proposed; operator-tunable per secret via `secrets.rotation_grace`. Real-world auth rotations typically run 24h to 7d for partner-shared keys (gives partners time to update their side); shorter for JWT/HMAC where we control both sides.

**What this unblocks (a single piece of infrastructure closes multiple gaps):**

- **#7 from backlog: DSP + SSP CRUD auth** — middleware looks up the caller's `api_keys` row (or JWT signing key from `secrets`) in the warm cache, validates, sets `account_id` in context. Half-day of work once secrets infra exists.
- **External Prebid partner auth** — partner sends API key in `X-API-Key` header; exchange's secrets cache validates against `purpose=partner_shared` rows. Required before external Prebid Server onboarding.
- **Rotatable JWT signing keys** — gateway currently has one static signing key. New flow: rotate via UI, gateway re-loads, in-flight tokens validated against active+rotating until grace expires.
- **Tracker HMAC rotation** — currently the HMAC secret is config-as-static. With this infra, operator rotates in seconds without redeploys.

**What this does NOT solve:**

- **Service-to-service mTLS** — that's a K8s service mesh decision (Linkerd / Istio). Separate axis, defer until prod requires it. The secrets table could store shared secrets for an HMAC-based S2S auth interim solution if needed before mesh adoption.

**Implementation phases (in order, sized for separate commits / sessions):**

| # | What | Touches | Size |
|---|---|---|---|
| 1 | Migration 027: `secrets` table + RLS + indexes. `pkg/secrets/` package with `SecretLoader` for warm cache + per-service filter helpers. | `migrations/027_*.sql`, `pkg/secrets/`, `pkg/store/postgres/secrets.go` | S–M |
| 2 | Each service wires its own warm cache (gateway: jwt_signing; tracker: hmac_tracker; exchange: partner_shared; pubad: service_s2s as needed). Readiness gate on first load. | every `cmd/<svc>/main.go` | M |
| 3 | Generic auth middleware: `middleware.AuthAPIKey(cache)`, `middleware.AuthJWTRotatable(cache)`. Uses warm cache, accepts active+rotating values. | `pkg/middleware/auth.go` | M |
| 4 | DSP + SSP CRUD endpoints wrap with the new middleware. E2E tests: unauth'd → 401, authed → 200, wrong-tenant → 403. | `cmd/dsp/management.go`, `cmd/ssp/management.go`, e2e | S |
| 5 | Bootstrap root flow: env-driven `PLATFORM_ROOT_PASSWORD` → one-shot `/v1/auth/bootstrap` → creates first admin. | `cmd/gateway/`, schema for operator accounts | S |
| 6 | Config manager UI "Secrets" tab: list / create / rotate / revoke. Audit trail rows in `audit_log` for every secret change. | `web/templates/config/manager.html`, `pkg/config/manager.go` (new `/v1/secrets` API) | M |
| 7 | Rotation workflow + e2e tests: rotate a secret, prove old + new both validate during grace, prove old stops validating after grace, prove cache propagation < 1s via NATS. | e2e harness helpers + tests | M |

Total: 1–2 sessions of focused work. Worth doing as one cohesive piece rather than incrementally because Phase 4 (DSP+SSP auth) is the actual blocker for staging deployment and earlier phases are scaffolding.

**Blocked work:** Until this lands: no staging deployment (CRUD endpoints exposed unauth'd); no external Prebid partner onboarding; rotation of any secret requires a redeploy.

---

## Vision

A full-stack programmatic advertising platform in a single Go monorepo that a developer can clone and run locally end-to-end. Every component - from bid request to impression tracking - runs on one machine, giving complete visibility into the data flow with zero data slippage.

**The problem we solve:** At real ad tech companies, no single person can run or understand the full stack. Services are owned by different teams, data flows through opaque Spark jobs and third-party pipes, and debugging means guessing where events got lost. This project makes the entire pipeline transparent, testable, and runnable on a laptop.

**Key niche:** Fast local development and testing. A developer can trace a single ad request from publisher page load through auction, serving, impression, click, and conversion - all locally, all observable.

---

## Ad Tech Core Components

### 1. Demand-Side Platform (DSP)

**What it is:** The system advertisers use to buy ad inventory programmatically.

**Responsibilities:**
- Campaign creation and management (budgets, targeting, scheduling)
- Bid strategy configuration (CPM, CPC, CPA goals)
- Audience targeting (geo, device, demographics, behavioural segments)
- Creative management (uploading and associating ad creatives with campaigns)
- Real-time bidding (RTB) - evaluating bid requests and submitting bid responses
- Budget pacing - spreading spend evenly over a campaign's lifetime
- Reporting dashboard for advertisers (spend, impressions, clicks, conversions, CTR, ROI)

### 2. Supply-Side Platform (SSP)

**What it is:** The system publishers use to manage and sell their ad inventory.

**Responsibilities:**
- Publisher onboarding and inventory registration (sites, apps, ad placements)
- Ad slot configuration (sizes, formats, floor prices)
- Sending bid requests to the ad exchange
- Selecting the winning bid and returning the ad creative to the publisher
- Revenue reporting for publishers
- Quality controls (block lists, category filters, ad quality enforcement)

### 3. Ad Exchange

**What it is:** The central marketplace connecting DSPs and SSPs, running the real-time auction.

**Responsibilities:**
- Receiving bid requests from SSPs
- Broadcasting bid requests to eligible DSPs
- Running the auction (first-price or second-price)
- Determining the winner and notifying both sides
- Enforcing auction rules and deal terms
- Handling timeouts (DSPs that don't respond in time are excluded)

### 4. Ad Server

**What it is:** The system that delivers the ad creative to the end user's browser.

**Responsibilities:**
- Storing and serving ad creatives (images, HTML, native)
- Rendering the correct ad based on the auction winner
- Generating impression, click, and viewability tracking pixels/URLs
- Handling ad fallback/default ads when no auction winner exists
- Frequency capping enforcement at serve time

### 5. Tracker (Event Pipeline)

**What it is:** The data collection layer that captures every event in the ad lifecycle.

**Responsibilities:**
- Impression tracking (ad was rendered/viewable)
- Click tracking (user clicked the ad)
- Conversion tracking (user completed a desired action post-click or post-view)
- Event ingestion (high-throughput, low-latency)
- Event deduplication and basic fraud filtering
- Feeding data to billing, reporting, and optimisation systems

### 6. Reporting and Analytics

**What it is:** The aggregation layer that turns raw events into dashboards and metrics.

**Responsibilities:**
- Real-time and historical dashboards
- Metrics: impressions, clicks, CTR, conversions, spend, eCPM, ROAS
- Breakdowns by campaign, creative, placement, geo, device, time
- Publisher revenue reports
- Data exports and API access

**Current state of event dispatch** (cmd/reporting):

Per-subject handlers attached via `EventConsumer.RegisterNATSSubscriptions`. After each NATS message arrives, two destinations are written:

| Event subject | Analytics store | Billing ledger |
|---|---|---|
| `adtech.events.impression` | `InsertImpression` (always) | `ProcessEvent` — CPM bills immediately, CPC/CPA/vCPM/CPCV reserve budget |
| `adtech.events.click` | `InsertClick` (always) | `SettleByTrace(traceID, "click")` (wired 2026-06-01) — no-op when reservation absent (CPM) or wrong model |
| `adtech.events.conversion` | `InsertConversion` (always) | `SettleByTrace(traceID, "conversion")` (wired 2026-06-01) — same no-op semantics |
| `adtech.events.view` (viewability) | `InsertView` (wired 2026-06-01) — server-authoritative IAB calc, persisted with `iab_viewable` verdict | `SettleByTrace(traceID, "viewable")` when `IABViewable=true` (wired 2026-06-01). Non-viewable views leave reservation open until the expiry cron lands. |
| `adtech.auction.complete` | `InsertAuction` (always) | not billed (the corresponding `adtech.auction.win` event is what triggers billing) |

The reserve/settle dispatch logic exists in `pkg/billing.Engine.ProcessEvent` — switches on `BidModel`. What's missing is the call to it from the click/conversion/view handlers. This is why all 8 `tests/e2e/billing_models_test.go` cases skip with "CPC/CPA reserve-settle dispatch in cmd/reporting consumer pending."

**Current backing stores** (both pluggable, both in-memory in dev):

- **Analytics**: `analytics.NewMemory()` in dev. `pkg/store/analytics/duckdb.go` exists and works (CGO build via `build/Dockerfile.reporting`), used in staging. ClickHouse impl planned for prod (multi-writer).
- **Billing ledger**: `billing.NewLedger()` is an in-process `[]LedgerEntry` slice. Volatile — restart wipes it. Accumulates across the e2e suite's lifetime (test `Reset` truncates Postgres + flushes Redis but doesn't reset the reporting process). Postgres-backed ledger is the planned upgrade — schema columns are in `migrations/` (`ledger_entries`, `invoices`, `reservations`) but nothing writes to them yet. Until then, every spend-tracking test that runs against a live stack should snapshot `TotalSpend` before its action and assert on the delta, not the absolute. Existing examples: `TestEndToEnd/08_tracker_and_billing`, `TestFraudDedupSameImpressionDropped`.

**Downstream consumers of the analytics store** (scaffolded, not wired):

- `cmd/rollup` — minute/hour/day/month aggregates for fast dashboard queries. Job stub.
- `cmd/webhooks` — ✅ SHIPPED: subscribes to `budget.depleted`/`balance.depleted`/`campaign.state_changed`, POSTs HMAC-signed envelopes to account-registered URLs with retries + a `webhook_deliveries` log.
- `cmd/pipeline` enrichment — derived fields (fraud score, IAB classification) before events land in analytics. Stub.
- Billing persistence — `Ledger.Record` should `INSERT INTO ledger_entries` in addition to the in-memory append. Not done.

### 7. User and Account Management

**What it is:** Authentication, authorisation, and multi-tenant account management.

**Responsibilities:**
- Advertiser and publisher account registration
- Role-based access control (admin, manager, viewer)
- API key management
- Organisation/team structures

**Current state (2026-05-31):** the user/account *tables* exist (`accounts`, `api_keys`, `team_members`) and RLS uses `account_id` for tenant isolation, but the *runtime auth surface* is not built. No middleware checks bearer tokens / API keys; no service distinguishes "anonymous" from "authenticated" callers; no per-role permissions are enforced. Today's effective security model is "trusted network" — fine for local dev where only the developer's machine talks to the services, but a hard blocker for any external surface.

**What this blocks (today):** the campaign-management endpoints landed on `cmd/dsp/main.go` (`POST/PATCH/DELETE /v1/dsp/campaigns`) are real platform endpoints but ship unauthenticated. They're safe in local dev (closed network, dev tooling only), and the e2e suite exercises them, but they MUST NOT be exposed to any public surface (or even an internal staging network) until auth lands. The handler files contain explicit TODO comments pointing to this section.

**What needs to land:**

1. `pkg/middleware/auth.go` — bearer token / mTLS verification middleware. Reads expected token from config (secret tier), wraps any handler requiring auth. Returns 401 on missing/invalid token.
2. Identity-bearing context: middleware decodes the token to `(user_id, account_id, roles)` and attaches to `r.Context()`. Handlers read identity via a helper (`auth.IdentityFromContext`) and pin tenant context to the caller's account.
3. Per-role permissions: handler-level checks like `if !auth.HasRole(ctx, "admin")` for mutating operations. Roles loaded from `team_members` table.
4. Audit log wiring: every mutation goes into `audit_log` with caller identity and before/after diff. Table already exists in `migrations/`.
5. Apply to the campaign management endpoints + every future POST/PATCH/DELETE that lands on a service-direct route.
6. **Same auth primitives serve the External DSP Partners outbound case** (see "External DSP Partners → Auth-aware HTTP client") — same `pkg/middleware/auth.go` design covers both inbound (we receive a token) and outbound (we send a token to a partner). Build both directions in the same module to keep one consistent auth story.

### 8. Data and Audience Management

**What it is:** The layer that manages audience segments and targeting data.

**Responsibilities:**
- Audience segment creation and management
- Retargeting pools (users who visited a site, viewed a product, etc.)
- First-party data ingestion (simulated in local dev)

---

## System Diagrams

All diagrams are **code-based** and version controlled. Mermaid diagrams render directly in GitHub markdown. D2 diagrams generate SVGs via `make diagrams`.

### Diagramming Tools

| Tool | Used for | Files |
|---|---|---|
| **Mermaid** | Flow diagrams, sequence diagrams, ER diagrams (in markdown) | Inline in `.md` files |
| **D2** | Comprehensive architecture diagram (complex, 20+ services) | `docs/diagrams/*.d2` -> generates `docs/diagrams/*.svg` |

### High-Level Architecture (Mermaid)

```mermaid
graph TB
    subgraph External
        User[End User / Browser]
        AdvUI[Advertiser Dashboard]
        PubUI[Publisher Dashboard]
    end

    subgraph Ingress
        Traefik[Traefik - TLS Termination]
    end

    subgraph Services
        Gateway[Gateway - Auth, API, HTMX]
        DSP[DSP - Campaigns, Bidding, Budget]
        SSP[SSP - Publishers, Placements, Deals]
        Exchange[Exchange Cluster - Auctions]
        AdServer[Ad Server - Creatives, Serving]
        Tracker[Tracker - Events, Pixels]
        Reporting[Reporting + Billing]
        Pipeline[Pipeline - Data Ingest]
        SSAI[SSAI Stitcher]
        Transcoder[Transcoder]
        Webhooks[Webhook Dispatcher]
    end

    subgraph Infrastructure
        Postgres[(PostgreSQL)]
        Redis[(Redis)]
        NATS[NATS JetStream]
        Minio[(Minio / S3)]
        DuckDB[(DuckDB / ClickHouse)]
    end

    subgraph Observability
        Prometheus[Prometheus]
        Grafana[Grafana]
        Loki[Loki]
        Jaeger[Jaeger]
    end

    User -->|pixels| Traefik
    AdvUI --> Traefik
    PubUI --> Traefik

    Traefik -->|/v1/api/*| Gateway
    Traefik -->|/v1/t/*| Tracker
    Traefik -->|/v1/openrtb/*| Exchange

    Gateway -->|HTTP proxy| DSP
    Gateway -->|HTTP proxy| SSP
    Gateway -->|HTTP proxy| Reporting
    Gateway -->|HTTP proxy| AdServer

    SSP -->|gRPC internal| Exchange
    Exchange -->|gRPC ours / OpenRTB HTTP 3rd-party| DSP
    SSP -->|gRPC internal| AdServer
    AdServer -->|signed pixel URLs, browser fires| Tracker

    DSP --> Redis
    DSP --> Postgres
    SSP --> Postgres
    AdServer --> Minio
    AdServer --> Redis

    Tracker --> NATS
    Exchange --> NATS
    DSP --> NATS

    NATS --> Reporting
    Reporting --> DuckDB
    Reporting --> Postgres

    Pipeline --> Minio
    Pipeline --> DuckDB

    SSAI --> Exchange
    SSAI --> Minio
    Transcoder --> Minio

    NATS --> Webhooks

    Prometheus -.->|scrape| Services
    Loki -.->|collect| Services
    Grafana -.->|query| Prometheus
    Grafana -.->|query| Loki
    Grafana -.->|query| Jaeger
```

### Ad Request Flow (Mermaid Sequence Diagram)

```mermaid
sequenceDiagram
    participant User as End User Browser
    participant SSP as SSP
    participant Exchange as Exchange
    participant DSP as DSP
    participant AdServer as Ad Server
    participant Tracker as Tracker
    participant NATS as NATS JetStream
    participant Reporting as Reporting + Billing

    User->>SSP: Page load (ad tag fires)
    SSP->>Exchange: gRPC RunAuction (OpenRTB bid request)

    par Fan out to DSPs
        Exchange->>DSP: gRPC Bid (ours) / OpenRTB POST /bid (3rd-party)
        Note over DSP: Pacing check -> Targeting -> Bid modifiers -> Shading
        DSP-->>Exchange: Bid response ($2.80)
    end

    Note over Exchange: Run auction (first-price)
    Exchange-->>SSP: Auction response (winner)
    SSP->>AdServer: gRPC Serve(winner)
    Note over AdServer: Select creative, frequency cap check, macro substitution

    AdServer-->>SSP: Creative HTML + tracking URLs
    SSP-->>User: Ad creative rendered

    Exchange->>NATS: AuctionWinEvent (clearing_price)
    Exchange->>DSP: Win notice (clearing_price)

    User->>Tracker: Impression pixel fires
    Tracker->>NATS: ImpressionEvent

    opt User clicks ad
        User->>Tracker: Click redirect
        Tracker->>NATS: ClickEvent
        Tracker-->>User: 302 redirect to landing page
    end

    NATS->>Reporting: Consume events
    Note over Reporting: Write to analytics store + accrue billing
    NATS->>DSP: Budget decrement (DECRBY)
```

### NATS Event Flow (Mermaid)

```mermaid
graph LR
    subgraph Publishers
        Tracker[Tracker]
        Exchange[Exchange]
        DSP[DSP]
        AdServer[Ad Server]
        Gateway[Gateway]
        Pipeline[Pipeline]
        Billing[Reporting/Billing]
    end

    subgraph Streams
        EVENTS[EVENTS Stream]
        AUCTIONS[AUCTIONS Stream]
        BUDGET[BUDGET Stream]
        WEBHOOKS[WEBHOOKS Stream]
        PRIVACY[PRIVACY Stream]
        VIDEO[VIDEO Stream]
    end

    subgraph Consumers
        ReportingC[Reporting Consumer]
        DSPConsumer[DSP Consumer]
        WebhookC[Webhook Dispatcher]
        AudienceC[Audience Store]
        DeletionC[Privacy Deletion Job]
    end

    Tracker -->|impression, click, view, conversion| EVENTS
    Exchange -->|auction.win, auction.complete| AUCTIONS
    DSP -->|budget.depleted| BUDGET
    Gateway -->|privacy.opt_out| PRIVACY
    Pipeline -->|file.ingested, drift.detected| EVENTS
    AdServer -->|creative.review_completed| EVENTS

    EVENTS --> ReportingC
    AUCTIONS --> ReportingC
    AUCTIONS --> DSPConsumer
    BUDGET --> ReportingC
    WEBHOOKS --> WebhookC
    PRIVACY --> DeletionC
    EVENTS --> AudienceC
```

### Database Entity Relationships (Mermaid ER Diagram)

```mermaid
erDiagram
    Account ||--o{ InsertionOrder : has
    Account ||--o{ Publisher : "is a"
    Account ||--o{ TeamMember : has
    Account ||--o{ APIKey : has

    InsertionOrder ||--o{ LineItem : contains
    LineItem ||--o{ LineItemCreative : has
    Creative ||--o{ LineItemCreative : "attached to"
    LineItem ||--o{ TargetingRule : has
    LineItem ||--o{ BidModifier : has
    LineItem ||--o{ FrequencyCap : has

    Publisher ||--o{ Placement : owns
    Placement ||--o{ Deal : "sold via"
    Placement ||--o{ QualityControl : has

    Account {
        uuid id PK
        text name
        text type "advertiser|publisher|agency"
        text currency
    }

    InsertionOrder {
        uuid id PK
        uuid account_id FK
        decimal budget
        date start_date
        date end_date
        text status
    }

    LineItem {
        uuid id PK
        uuid insertion_order_id FK
        text bid_strategy
        decimal base_bid
        text pacing_mode
        text status
    }

    Creative {
        uuid id PK
        uuid account_id FK
        text format "display|native|video|audio"
        text review_status
    }

    Placement {
        uuid id PK
        uuid publisher_id FK
        text format
        decimal floor_price
    }
```

### Comprehensive Architecture Diagram (D2)

For the full system with all 20+ services, infrastructure, data flows, and channel-specific exchanges, we use D2 because Mermaid can't handle the complexity.

**D2 source file:** `docs/diagrams/architecture.d2`

```d2
# docs/diagrams/architecture.d2
# Generate: d2 docs/diagrams/architecture.d2 docs/diagrams/architecture.svg

direction: right

internet: Internet {
    shape: cloud
}

traefik: Traefik {
    shape: hexagon
    style.fill: "#4DC0B5"
}

internet -> traefik: HTTPS

# --- External-facing services ---
gateway: Gateway {
    style.fill: "#6366F1"
    auth: Auth + RBAC
    api: REST API
    htmx: HTMX Dashboard
    swagger: Swagger UI
}

tracker: Tracker {
    style.fill: "#EF4444"
    imp: /v1/t/imp
    click: /v1/t/click
    conv: /v1/t/conv
    view: /v1/t/view
    video: /v1/t/video
}

exchange: Exchange Cluster {
    style.fill: "#F59E0B"
    display: Display
    video: Video/Audio
    dooh: DOOH
    retail: Retail
    game: In-Game
}

traefik -> gateway: /v1/api/*
traefik -> tracker: /v1/t/*
traefik -> exchange: /v1/openrtb/*

# --- Internal services ---
dsp: DSP {
    style.fill: "#10B981"
    campaigns: Campaigns
    targeting: Targeting
    bidding: Bidding
    pacing: Pacing
    budget: Budget
}

ssp: SSP {
    style.fill: "#8B5CF6"
    placements: Placements
    deals: Deals
    quality: Quality Controls
}

adserver: Ad Server {
    style.fill: "#EC4899"
    creatives: Creatives
    serving: Serving
    macros: Macros
    freqcap: Freq Cap
}

reporting: Reporting + Billing {
    style.fill: "#06B6D4"
    analytics: Analytics
    billing: Billing
    reports: Reports
    rollups: Rollups
}

pipeline: Pipeline {
    style.fill: "#84CC16"
    ingest: Ingest
    validate: Validate
    normalise: Normalise
    enrich: Enrich
}

ssai_svc: SSAI Stitcher {
    style.fill: "#F97316"
    manifest: Manifest Manipulation
    sessions: Session Manager
    beacons: Beacon Server
}

transcoder: Transcoder {
    style.fill: "#A855F7"
}

webhooks: Webhooks {
    style.fill: "#64748B"
}

# --- service-to-service connections ---
gateway -> dsp: HTTP proxy
gateway -> ssp: HTTP proxy
gateway -> reporting: HTTP proxy
gateway -> adserver: HTTP proxy

ssp -> exchange: gRPC (internal)
exchange -> dsp: gRPC (ours) · OpenRTB HTTP (3rd-party)
ssp -> adserver: gRPC (internal)
adserver -> tracker: signed pixel URLs (browser fires)
ssai_svc -> exchange: HTTP (VAST)

# --- Infrastructure ---
postgres: PostgreSQL {
    shape: cylinder
    style.fill: "#336791"
    primary: Primary
    standby: Standby
}

pgbouncer: PgBouncer {
    style.fill: "#336791"
}

redis: Redis {
    shape: cylinder
    style.fill: "#DC382D"
}

nats: NATS JetStream {
    shape: queue
    style.fill: "#27AAE1"
}

minio: Minio / S3 {
    shape: cylinder
    style.fill: "#C72C48"
}

duckdb: DuckDB / ClickHouse {
    shape: cylinder
    style.fill: "#FFC107"
}

# --- Service to infra connections ---
dsp -> pgbouncer
ssp -> pgbouncer
gateway -> pgbouncer
reporting -> pgbouncer
pipeline -> pgbouncer
pgbouncer -> postgres

dsp -> redis
adserver -> redis
tracker -> redis
reporting -> redis

tracker -> nats
exchange -> nats
dsp -> nats
nats -> reporting
nats -> webhooks

adserver -> minio
pipeline -> minio
transcoder -> minio
ssai_svc -> minio

reporting -> duckdb

# --- Observability ---
prometheus: Prometheus {shape: cylinder; style.fill: "#E6522C"}
grafana: Grafana {style.fill: "#F46800"}
loki: Loki {shape: cylinder; style.fill: "#F46800"}
jaeger: Jaeger {style.fill: "#66CFE0"}

prometheus -> grafana
loki -> grafana
jaeger -> grafana
```

**Generate SVG:** `d2 docs/diagrams/architecture.d2 docs/diagrams/architecture.svg`

### Diagram Directory

```
docs/
    diagrams/
        architecture.d2         # Full system architecture (D2 source)
        architecture.svg        # Generated SVG (committed, auto-generated)
        Makefile                # make diagrams -> regenerates all SVGs from .d2 files
```

**Makefile target:**
```makefile
diagrams:
	d2 docs/diagrams/architecture.d2 docs/diagrams/architecture.svg
```

**CI check:** GitHub Actions verifies that committed SVGs match the D2 source. If a developer changes the `.d2` file but forgets to regenerate, CI fails.

### How These Components Interact

1. A user visits a publisher's page. The SSP identifies an available ad slot.
2. The SSP sends a bid request to the Ad Exchange with slot details (size, page context, user signals).
3. The Ad Exchange fans out the bid request to eligible DSPs via OpenRTB HTTP.
4. Each DSP evaluates the request: pacing check -> targeting -> bid modifiers -> bid shading -> submit bid.
5. The Ad Exchange runs the auction (strategy depends on channel) and selects winner(s).
6. The winning ad creative is passed to the Ad Server, which assembles the ad markup (macro substitution, third-party pixels, frequency cap check).
7. The creative is served to the user's browser.
8. The Tracker captures the impression pixel. If the user clicks, that is tracked too. If they later convert, that is attributed back.
9. All events flow through NATS JetStream to the Reporting service, which writes to both the analytics store and billing tables atomically.
10. Grafana dashboards show real-time metrics. Deployment annotations overlay on all charts.

---

## Technical Approach

| Concern | Approach |
|---|---|
| Language | Go for everything (backend, frontend, tooling). One language = no context switching, no duplicate packages, any dev can work on any service. Python only where Go is genuinely the wrong tool (e.g. data science / ML work). |
| Monorepo | Single Go module with `cmd/` per service, shared `internal/` packages |
| Frontend | Go server-side rendering with HTMX + Tailwind CSS. No JS build pipeline, no Node.js. Dashboards are just Go handlers returning HTML fragments. |
| Real-time bidding | Low-latency HTTP service with strict timeout enforcement |
| Event bus | NATS JetStream - persistent, ack-based delivery, dead letter queues, replay. Behind an interface in `pkg/events/` so Kafka can be swapped in later without service changes. |
| Database | PostgreSQL for transactional data. Analytics store is pluggable: DuckDB (embedded, zero infra, ideal for local/staging) or ClickHouse (server-based, scales to billions, ideal for prod). Both supported behind an interface. |
| Object storage | S3 everywhere. Minio (S3-compatible) locally, real S3 in staging/prod. Same code, same SDK, just a different endpoint URL via Kustomize overlay. No filesystem implementation needed. |
| Internal comms | Protobuf/gRPC for all service-to-service calls. Proto definitions in `pkg/proto/`, generated Go code shared across services. |
| External/bidding | OpenRTB JSON/HTTP for auction bidding (industry standard). |
| Async events | NATS with protobuf-encoded messages for event pipeline (tracker -> reporting, budget updates). |
| Frontend API | HTTP/JSON for HTMX dashboard endpoints. |
| Caching | Layered: L1 in-process (Go maps) for read-heavy config data, L2 Redis for shared mutable state (budgets, frequency caps), L3 Postgres as source of truth. Cache invalidation via NATS pub/sub. |
| Auth | JWT-based authentication, RBAC |
| Infrastructure | Kubernetes everywhere - Colima + k3s locally, k3s/K8s in production. Kustomize for environment overlays (no Helm). Same manifests, same tooling, one system to learn. No Docker Compose. |
| Dev orchestration | Tilt - watches Go files, auto-rebuilds and hot-deploys into k3s. Dashboard shows build status, logs, and health for all services. Tiltfile defines custom buttons (seed data, fire test requests, reset DB). |
| Testing | Integration tests that trace a full ad request end-to-end |
| Observability | Tiered approach - see Observability section below |

---

## Observability

### Tier 1 - From Day One

**Structured logging (slog)**
- Go's stdlib `log/slog` with JSON output
- Every log line includes: trace ID, service name, timestamp, log level
- All services use the shared `pkg/logger` package for consistent format

**Trace IDs across services**
- A single trace ID is generated when the SSP creates a bid request
- The ID is passed via HTTP headers (`X-Trace-ID`) through every service in the chain: SSP -> Exchange -> DSP -> Ad Server -> Tracker -> Reporting
- Grep one trace ID across all service logs and see the full lifecycle of a single ad request
- This is the primary tool for debugging data slippage - if an event is missing, the trace ID tells you exactly where it dropped

### Tier 2 - Once Services Are Running

**Prometheus metrics**
- Each service exposes a `/metrics` endpoint
- Standard metrics per service: request count, latency histograms (p50/p95/p99), error rates
- Ad-tech-specific metrics:
  - Exchange: auctions/sec, bid response times, win rates, timeout rates, no-bid rates
  - DSP: bids submitted, budget remaining, pacing rate
  - Tracker: events/sec by type (impression, click, conversion), dedup rate
  - Ad Server: creatives served/sec, fallback rate
- Prometheus runs as a service in the k3s cluster, scrapes all services

**Grafana dashboards**
- Pre-built dashboards shipped in the repo (`k8s/base/grafana/`)
- Pipeline health dashboard: end-to-end flow from bid request to impression, spot bottlenecks at a glance
- Per-service dashboards: detailed metrics for each service
- Runs in the k3s cluster alongside everything else - available locally and in staging/prod

### Tier 3 - When Needed

**OpenTelemetry tracing (Jaeger)**
- Distributed traces with span visualisation
- See the exact timing of SSP -> Exchange -> DSP -> Ad Server as a waterfall diagram
- Identify latency bottlenecks across service boundaries
- Jaeger runs as a service in the k3s cluster
- Instrumented via OpenTelemetry Go SDK in `pkg/middleware`

**Alerting**
- Grafana alerting rules for critical conditions:
  - No impressions received in X minutes
  - Auction latency exceeds threshold
  - Event pipeline lag growing
  - Service error rate spike
- Only relevant for staging/prod - not needed locally

---

## Monorepo Structure (Planned)

```
ad-tech-mono/
  cmd/
    dsp/            # DSP service entrypoint
    ssp/            # SSP service entrypoint
    exchange/       # Ad Exchange service entrypoint
    adserver/       # Ad Server service entrypoint
    tracker/        # Event tracking service entrypoint
    reporting/      # Reporting + Billing unified service (--mode=service | --mode=scheduler)
    gateway/        # API gateway / auth entrypoint
    seed/           # CLI tool to load seed data profiles into the database
    simulator/      # CLI tool to generate fake ad traffic based on simulation profiles
    pipeline/       # Data pipeline service (ingest, validate, normalise, enrich)
    rollup/         # Rollup job (K8s CronJob, aggregates data by time granularity)
    migrate/        # Database migration runner (embeds SQL files, runs goose)
    optimise/       # Optimisation pipeline jobs (bid optimisation, placement scoring, etc.)
    fraud/          # Batch fraud detection job (pattern analysis, scoring updates)
    adstxt/         # ads.txt crawler CronJob (fetches and caches publisher ads.txt files)
    privacy-delete/ # User data deletion job (scans and anonymises across all stores)
    privacy-verify/ # Deletion verification CronJob (confirms deletion completeness)
    ssai/           # SSAI Stitcher - manifest manipulation, segment proxying, break detection
    transcoder/     # Video transcoder - converts ad creatives to stream-compatible variants (K8s Job)
    cleanroom/      # Clean room computation job runner (isolated K8s Job, no network access)
    webhooks/       # Webhook dispatcher - consumes NATS events, delivers HTTP POST to registered URLs
  pkg/                # Shared packages - all reusable libraries live here, nothing external
    models/           # Shared domain models (Campaign, Creative, Placement, BidRequest, etc.)
    auction/          # Auction logic (first-price, second-price)
    targeting/        # Targeting evaluation engine
    pacing/           # Budget pacing logic
    deals/            # Deal types (PMP, PG, preferred), deal matching, priority logic
    identity/         # Identity graph - matching, merging, cross-device linking
    audience/         # Audience segments, CRM uploads, advertiser matching
    privacy/          # Consent checking, opt-out, right-to-deletion propagation
    email/            # Email sending interface + template rendering (Mailpit / SES)
    cleanroom/        # Clean room computation engine - overlap, expansion, composition
    marketplace/      # Data marketplace - listings, expansion estimates, barter, fairness scoring
    dooh/             # DOOH audience estimation, proof-of-play verification
    retail/           # Product catalog sync, relevance scoring, keyword bidding, catalog creatives
    simulator/        # Programmable simulator Go library (used by tests and CLI)
    chaos/            # Chaos testing - pod kill, network injection, latency injection, verification
    openrtb/          # OpenRTB 2.6 request/response types (pinned version) - site, app, regs, native
    currency/         # Multi-currency conversion, exchange rates, daily rate updates
    adserving/        # Macro substitution, third-party pixel piggybacking, frequency cap checks, VAST/VMAP generation
    ssai/             # SSAI session manager, beacon firing, manifest manipulation, SCTE-35 detection
    store/            # Database access layer
      postgres/       # PostgreSQL transactional store
      analytics/      # Analytics store interface + DuckDB and ClickHouse implementations
      objects/        # S3 object storage client (works with Minio locally and real S3 in prod)
    events/           # Event types and message bus abstraction (EventBus interface)
      nats/           # NATS JetStream implementation of EventBus
    config/           # Shared configuration loading
    middleware/       # HTTP middleware (auth, logging, tracing, CORS)
    logger/           # Structured logging setup
    proto/            # Protobuf definitions + generated Go code (single source of truth)
    clock/            # Clock interface (Real for prod, Fake for tests) - no time.Now() in app code
    health/           # Shared health check library (/healthz, /readyz, dependency registration)
    lifecycle/        # Shared graceful shutdown, drain, flush logic
    billing/          # Spend calculation, invoice models, reconciliation engine
    reporting/
      builder.go      # Custom report query construction from user selections
      templates.go    # Pre-built report definitions
    audit/            # Audit logging library, actor context extraction
    fraud/            # Real-time fraud checks, scoring, bot/IP lists
      realtime.go     # Middleware for tracker and exchange
      scoring.go      # Combine signals into fraud_score
      lists/          # Bot UA lists, data centre IP ranges
    cache/            # Layered cache - L1 in-process, L2 Redis, invalidation via NATS
      redis/          # Redis client wrapper, atomic counters
    pipeline/         # Format detection, validation, normalisation, enrichment logic
    store/datalake/   # Parquet read/write, Delta Log management
    store/rollup/     # Generic rollup engine - config-driven aggregation framework
    testutil/         # Shared test helpers, fixtures, end-to-end harness
  web/
    templates/      # Go HTML templates (used with HTMX)
      simulator/    # Publisher simulator page templates (news_site, ecommerce, mobile_app, etc.)
    static/         # Tailwind CSS, static assets
      adtech.js     # Publisher-facing ad tag SDK (sets platform ID, sends user data, viewability, native rendering)
      debug-overlay.js  # Developer debug panel for publisher simulator
  profiles/
    seed/             # Seed data profiles (JSON/YAML)
      minimal.yaml    # 1 advertiser, 1 campaign, 1 publisher - smoke testing
      standard.yaml   # Multiple advertisers/publishers, varied targeting - general dev
      stress.yaml     # Hundreds of campaigns, thousands of placements - perf testing
      demo.yaml       # Realistic branded data, pre-populated reporting - demos
    publishers/       # Per-publisher data format configs (field mappings, validation rules)
      acme_media.yaml
    simulation/       # Simulation profiles (JSON/YAML)
      trickle.yaml    # 1 req/sec, verbose logging - step-by-step debugging
      steady.yaml     # 50-100 req/sec, mixed traffic - day-to-day dev
      burst.yaml      # Spike patterns, traffic surges - pacing/timeout testing
      replay.yaml     # Replay captured bid request sequences - regression testing
    fraud/              # Fraud detection configuration
      rules.yaml        # Fraud scoring thresholds, weights, blocklists
    chaos/              # Chaos test scenarios
      redis_failure.yaml
      cascade_failure.yaml
  migrations/         # Database migrations
  k8s/
    base/               # Plain K8s manifests (deployments, services, configmaps)
      dsp/
      ssp/
      exchange/
        display/         # Display/native exchange (SingleWinner)
        video/           # Video/audio exchange (Pod + SingleWinner)
        dooh/            # DOOH exchange (TimeSlot)
        retail/          # Retail media exchange (RelevanceWeighted)
        game/            # In-game exchange (Batch + SingleWinner)
      adserver/
      tracker/
      reporting/
      gateway/
      postgres/
      nats/
      redis/            # Redis deployment (L2 cache - budgets, frequency caps, sessions)
      pgbouncer/        # PgBouncer connection pooler (primary + standby pools)
      minio/            # Minio S3-compatible object storage (local dev)
      clickhouse/       # ClickHouse deployment (prod/heavy workloads only)
      pipeline/         # Pipeline service deployment
      webhooks/         # Webhook dispatcher service deployment
      seed/             # Seed job (run via Tilt button or manually)
      simulator/        # Simulator job (run via Tilt button or manually)
      migrate/          # Migration K8s Job (runs before service deployments)
      prometheus/       # Prometheus deployment + scrape config
      grafana/          # Grafana deployment + pre-built dashboards
      jaeger/           # Jaeger deployment (Tier 3)
      loki/             # Loki log aggregation
      promtail/         # Promtail DaemonSet (collects container logs)
      ssai/             # SSAI stitcher service deployment
      transcoder/       # Transcoder coordinator deployment
      mailpit/          # Mailpit fake SMTP + web UI (local/staging)
      ingress/          # Traefik IngressRoute manifests (routing rules)
    cronjobs/
      rollup-minute/    # Minute rollup CronJob
      rollup-hourly/    # Hourly rollup CronJob
      rollup-daily/     # Daily rollup + purge CronJob
      rollup-monthly/   # Monthly rollup + purge CronJob
      optimise/         # Optimisation pipeline CronJobs (bid, placement, creative, budget)
      fraud-batch/      # Batch fraud detection CronJob
      report-scheduler/ # Runs saved scheduled reports + daily invoice/payout generation (cmd/reporting --mode=scheduler)
      adstxt/           # ads.txt / app-ads.txt crawler CronJob
      privacy-delete/   # User data deletion job (on-demand)
      privacy-verify/   # Deletion verification CronJob (daily)
      backup-postgres/  # Daily Postgres backup CronJob
      backup-duckdb/    # Daily DuckDB file backup CronJob
    overlays/
      local/            # Kustomize patches for local k3s (single replicas, debug logging, filesystem storage)
      staging/          # Kustomize patches for staging (dev S3, reduced replicas, real-ish config)
      prod/             # Kustomize patches for production (replicas, resource limits, prod secrets, prod S3)
  build/
    Dockerfile          # Shared multi-stage Dockerfile for all Go services (CGO_ENABLED=0)
    Dockerfile.gateway  # Extended Dockerfile for gateway (embeds web/ assets)
    Dockerfile.reporting # CGO-enabled Dockerfile for reporting (DuckDB driver requires CGO)
    Dockerfile.transcoder # FFmpeg-enabled Dockerfile for video/audio transcoding
  Tiltfile              # Tilt orchestration - watches code, builds, deploys, custom dev buttons
  python/
    fraud/              # Fraud ML model training scripts
    optimisation/       # Optimisation model training (audience clustering, bid models)
  .github/
    workflows/
      ci.yml            # PR pipeline (lint, test, build, e2e, A/B)
      nightly.yml       # Full nightly (build, test, perf, chaos, security, summary)
      deploy-staging.yml # Auto-deploy to staging on merge
      deploy-prod.yml   # Manual prod deploy with approval gate
      perf-test.yml     # k6 performance regression
      chaos-test.yml    # Chaos resilience regression
  tests/
    k6/                 # k6 performance/load test scripts
      tracker-load.js
      exchange-load.js
      gateway-api-load.js
      ssai-load.js
      mixed-load.js
      thresholds.json
  infra/
    terraform/          # Cloud infrastructure as code (VMs, VPC, DNS, CDN, LB)
    runbooks/           # Operational runbooks for DR and incident response
  buf.yaml              # Buf configuration (linting, breaking change detection)
  buf.gen.yaml          # Buf code generation config
  Makefile
  go.mod
  go.sum
  docs/
    PLAN.md             # This file
    openapi.yaml        # OpenAPI 3.x spec for REST, OpenRTB, and Tracker endpoints
    CHANGELOG.md        # API changelog for external consumers
    diagrams/
      architecture.d2   # Full system architecture diagram (D2 source)
      architecture.svg  # Generated SVG (auto-generated from .d2)
      Makefile          # make diagrams -> regenerates SVGs
```

---

## What We Are Building (Scope)

**In scope (MVP):**
- DSP: campaign hierarchy (advertiser > insertion order > line item > creative), targeting with exclusions, bid modifiers, bid shading
- SSP: publisher/placement registration, deal management (PMP, PG, preferred), quality controls
- Ad Exchange: first-price auction (default), second-price as option, competitive separation, macro substitution
- Ad Server: display + native ad formats, creative serving, frequency capping (per-user/campaign/creative/placement/time-window), third-party pixel piggybacking
- Tracker: impression, click, conversion (including view-through), viewability tracking
- Reporting: dashboards with key metrics, reach and frequency, unique users
- Billing: multi-currency, variable margin/revenue share per publisher
- Auth: user accounts, login, API keys
- OpenRTB: `site` + `app` objects, `regs` object for consent signals, native ad response format
- End-to-end integration tests that trace a full request
- K8s manifests that run identically on local Colima + k3s and production
- Structured logging with trace IDs across all services

**Simulated / deferred:**
- Large-scale data processing (Spark jobs etc.) - simulated with simple Go aggregations
- Real ad network integrations (we own both sides)
- Video/audio ad formats (VAST/VPAID, DAAST, ad pods, outstream)
- Mobile-specific (MRAID, SKAdNetwork, IDFA/GAID, native mobile SDK)
- Header bidding (Prebid.js client-side, Prebid Server S2S)
- Advanced attribution (multi-touch, incrementality testing, offline conversion import)
- Compliance detail (TCF string parsing, COPPA handling, DSA, US state privacy laws, political ad transparency)
- Brand safety vendor integration (DoubleVerify, IAS, MOAT pre-bid signals)
- DMP/CDP integration (third-party audience data from BlueKai, LiveRamp, Segment)
- Agency trading desk support (multi-advertiser management)
- Operational maturity (SLA tracking, capacity planning, chaos engineering, runbooks)
- UI polish (i18n/localisation, WCAG accessibility, campaign reach estimator)
- Rewarded ads, interstitial ads, rich media / expandable ads
- OpenRTB SupplyChain (schain) object, Supply Path Optimisation (SPO)
- Tax calculations (VAT, sales tax, withholding tax)
- Payment gateway integration (Stripe, PayPal)

---

## Advertiser Workflows

### Campaign Lifecycle

Campaigns follow a state machine. Rules govern what can change in each state.

```
Draft -> Submitted -> In Review -> Approved -> Live -> Paused -> Ended -> Archived
                         |
                         +-> Rejected (with reason, can resubmit)
```

| State | What can change | Transitions to |
|---|---|---|
| Draft | Everything | Submitted |
| Submitted | Nothing (locked for review) | In Review |
| In Review | Nothing (platform reviewing) | Approved, Rejected |
| Rejected | Fix issues flagged in review | Submitted (resubmit) |
| Approved | Budget, bid strategy, schedule | Live (auto when flight date starts, or manual) |
| Live | Budget, bid strategy, targeting, creatives, schedule | Paused, Ended |
| Paused | Same as Live | Live (resume), Ended |
| Ended | Nothing | Archived |
| Archived | Nothing (read-only) | - |

### Creative Review and Approval

Every creative goes through review before it can be served:

```
Uploaded -> Auto-Scan -> Manual Review (if needed) -> Approved / Rejected
```

**Auto-scan checks:**
- Image dimensions match declared size
- File size within limits
- No malware / malicious scripts (HTML creatives)
- Content policy (basic image classification)
- Landing URL is reachable and not blocklisted

**Review outcomes:**

| Outcome | What happens |
|---|---|
| Auto-approved | Passes all automated checks, no manual review needed |
| Flagged for review | Auto-scan uncertain, queued for platform review |
| Rejected | Fails policy. Advertiser notified with reason. Can upload a replacement. |
| Approved | Creative can be associated with campaigns and served |

Only approved creatives can be attached to live campaigns. If a creative is later rejected (policy change, complaint), it's pulled from serving immediately.

### Creative A/B Testing

Multiple creatives per campaign. The ad server rotates between them and tracks performance:

- **Even rotation** - equal serving initially to gather data
- **Weighted rotation** - shift towards better-performing creatives over time
- **Winner selection** - after statistical significance, auto-promote the winner (or recommend via dashboard)

Metrics per creative: CTR, conversion rate, viewability. Surfaced in the campaign dashboard and via the reporting API.

### Campaign Scheduling

| Feature | How it works |
|---|---|
| Flight dates | Campaign runs between start and end date |
| Daily budget cap | Max spend per day, resets at midnight UTC |
| Lifetime budget cap | Max total spend for the campaign |
| Dayparting | Only serve ads during specified hours (e.g. 9am-5pm) |
| Day-of-week | Only serve on specified days (e.g. weekdays only) |
| Timezone | Schedule interpreted in advertiser's timezone |

### Budget Management

- **Pre-pay:** Advertiser tops up balance, campaigns draw from it
- **Credit terms:** Approved advertisers get a credit limit (net 30, net 60). Invoiced periodically.
- **Low balance alerts:** Webhook/email when balance drops below threshold
- **Auto-pause:** Campaigns auto-pause when balance hits zero (no overspend)

### Programmatic API (Advertisers)

All workflows are available via REST API with API key auth:

- `POST   /v1/api/campaigns/bulk` - create multiple campaigns from a single request
- `POST   /v1/api/creatives/bulk` - upload multiple creatives
- `PUT    /v1/api/campaigns/bulk` - bulk update (e.g. pause all campaigns matching a filter)
- `GET    /v1/api/campaigns/{id}/creatives/performance` - creative A/B test results
- `POST   /v1/api/budget/topup` - add funds to account balance

---

## Publisher Workflows

### Deal Management

Publishers can create deals to sell inventory at preferred terms:

| Deal type | How it works | Auction? | Price | Volume |
|---|---|---|---|---|
| **Open auction** | Any DSP can bid. Highest wins. | Yes | Market price | No guarantee |
| **Private marketplace (PMP)** | Publisher invites specific advertisers. Still an auction but limited participants. | Yes (restricted) | Floor price (usually higher) | No guarantee |
| **Programmatic guaranteed (PG)** | Fixed price, guaranteed volume. No auction. Publisher commits X impressions at $Y CPM. | No | Fixed | Guaranteed |
| **Preferred deal** | Fixed price, no volume commitment. Advertiser gets first look before open auction. | First look, then open auction | Fixed | No guarantee |

**Deal priority in the Exchange:**

```
Bid request arrives for a placement
    |
    v
1. Check PG deals - any guaranteed volume to fulfil? -> Serve PG ad (no auction)
    |
    v
2. Check Preferred deals - offer first look to preferred advertisers -> Accept or pass
    |
    v
3. Check PMP deals - run auction with invited DSPs only -> Winner or no-bid
    |
    v
4. Open auction - all eligible DSPs bid
```

**Deal API:**
- `POST   /v1/api/deals` - create deal (type, advertisers, price, volume, dates)
- `GET    /v1/api/deals` - list deals
- `PUT    /v1/api/deals/{id}` - update deal terms
- `DELETE /v1/api/deals/{id}` - cancel deal
- `GET    /v1/api/deals/{id}/performance` - deal performance (fill rate, revenue, volume delivered vs committed)

### Ad Quality Controls

Publishers control what ads appear on their properties:

| Control | What it does | Example |
|---|---|---|
| **Advertiser blocklist** | Block specific advertisers | "No ads from CompetitorX" |
| **Category blocklist** | Block ad categories | "No gambling, no adult" |
| **Creative blocklist** | Block specific creatives by ID | Reported ad, inappropriate content |
| **Advertiser allowlist** | Only allow specific advertisers (premium placements) | "Only Fortune 500 brands on homepage" |
| **Domain blocklist** | Block ads linking to specific domains | "No ads linking to scam sites" |

Controls are checked by the Exchange before including a DSP's bid in the auction. A bid that violates publisher controls is silently dropped.

### Floor Price Management

| Feature | How it works |
|---|---|
| Static floor | Fixed minimum CPM per placement |
| Time-based floors | Higher floor during peak hours (e.g. $3 CPM 9am-5pm, $1 CPM overnight) |
| Device-based floors | Different floor for mobile vs desktop |
| Geo-based floors | Higher floor for premium geos (US/UK vs rest of world) |
| Dynamic floors | Auto-adjusted based on historical clearing prices (via optimisation pipeline) |

### Inventory Forecasting

"How many impressions will placement X get next week?" Based on historical data.

- Used by publishers to sell PG deals (commit a volume they can actually deliver)
- Based on rolling averages from the analytics store
- Shown in publisher dashboard with confidence intervals
- API: `GET /v1/api/publishers/{id}/placements/{id}/forecast?days=7`

### Programmatic API (Publishers)

All workflows available via REST API:

- `POST   /v1/api/placements/bulk` - create/update thousands of placements
- `PUT    /v1/api/placements/bulk-floors` - update floor prices in bulk
- `POST   /v1/api/quality-controls/sync` - sync blocklists/allowlists from publisher's own systems
- `GET    /v1/api/publishers/{id}/fill-rate` - real-time fill rate per placement
- `GET    /v1/api/publishers/{id}/revenue` - revenue data for publisher's own dashboards

### Ad Tag Management

- Generate JS ad tags or server-side tags per placement
- Track which placements are active (tag installed and receiving requests)
- Integration health dashboard - last request seen, fill rate, errors
- Alert if a placement stops receiving requests (tag removed?)

---

## External DSP Partners (Multi-Tenant Integration Platform)

### What this section is

The exchange today speaks **OpenRTB on the wire** — the protocol primitives (bid request fan-out, win/loss notify, OpenRTB JSON shapes) are implemented and work against internal DSPs (`cmd/dsp` + the two competitor pods). The gap is everything *around* the wire: partner onboarding, auth, observability, financial reconciliation, compliance. That's the operational layer needed to run as a multi-tenant exchange — accepting bids from real external DSP partners (Xandr, DV360, TTD, smaller players) instead of only our own internal bidders.

This is part of the project scope (intentional learning exercise — building the operational layer is how you actually learn how an exchange works). Spec'd here so the work has a documented home.

### Current state

| Capability | Built | Notes |
|---|---|---|
| OpenRTB BidRequest serialization | ✅ | `pkg/openrtb/openrtb.go` — 20 types matching IAB 2.5/2.6 |
| HTTP fan-out to DSP endpoints (parallel goroutines) | ✅ | `cmd/exchange/main.go` `fanOutToDSPs` |
| BidResponse parsing | ✅ | Same path |
| HTTP nurl (win notify) | ✅ | `sendWinLossNotifications` |
| HTTP lurl (loss notify) with reason code | ✅ | Same |
| Per-call timeout via request context | ✅ | Auction deadline propagated via fanCtx |
| Smart router (adaptive skip of slow/no-bid DSPs across auctions) | ✅ | `pkg/optimise.SmartRouter` |
| Bid shading model | ✅ | `pkg/bidshading` |

### Production gaps (the work to schedule)

Eight categories, build order based on what each unlocks.

#### 1. DSP registry (foundation — build first)

Today: `exchange.dsp_endpoints` config = comma-separated URLs. Static, set at deploy time, no metadata.

Need: a Postgres table `partners` with one row per integrated DSP, the exchange reads from it instead of env config. Schema sketch:

```sql
CREATE TABLE partners (
    id              UUID PRIMARY KEY,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL,  -- 'dsp', 'agency', etc.
    status          TEXT NOT NULL,  -- 'sandbox', 'active', 'paused', 'terminated'
    endpoint_bid    TEXT NOT NULL,  -- their /openrtb/bid URL
    endpoint_nurl   TEXT,           -- if different from bid base
    auth_method     TEXT NOT NULL,  -- 'api_key', 'mtls', 'oauth2'
    auth_secret_ref TEXT NOT NULL,  -- ref to secret store, never the secret itself
    channels        TEXT[],         -- 'display', 'video', 'native'
    formats         TEXT[],         -- 'banner', 'vast', 'mraid'
    sizes           TEXT[],         -- '300x250', '728x90', etc.
    geos            TEXT[],         -- ISO country codes they bid on
    timeout_ms      INT DEFAULT 100,
    max_qps         INT,            -- rate limit
    contact_tech    TEXT,           -- engineering contact email
    contact_billing TEXT,
    sla_terms       JSONB,          -- response time SLA, fill rate floor, etc.
    onboarded_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Implementation: new `cmd/partner-portal` (admin UI for managing DSPs) + warm cache in exchange reading from this table (same pattern as `pkg/store/postgres.CampaignLoader`). DSP onboarding flow: register → sandbox (test traffic, no billing) → certify (pass acceptance tests) → active. Unlocks everything else — auth, observability, SLAs are all per-row in this table.

#### 2. Auth-aware HTTP client

Today: bare `http.Client`, no headers added on outbound. Inbound win/loss endpoints don't verify caller identity.

Need: `pkg/dsp-client/` (new) — wraps `http.Client`, takes a partner record, picks the auth method, signs requests:

- **API key**: `Authorization: Bearer {secret}` header (read from secret store via `auth_secret_ref`)
- **mTLS**: load client cert + key, configure TLS dialer per partner (common for tier-1 partners)
- **OAuth2 client credentials**: fetch token, cache until expiry, re-fetch on 401
- **Request signing** (for partners that require HMAC-signed request bodies)

Inbound side: tracker/exchange win endpoints verify signed callbacks before crediting budget.

Secret store: should not be Postgres columns — use K8s secrets, Vault, or AWS Secrets Manager via a `pkg/secrets/` abstraction. The partner row holds a *reference* (`auth_secret_ref = "vault:partners/xandr/api_key"`), not the value.

#### 3. Per-partner observability

Today: smart-router metrics are aggregate (all DSPs combined). Logs include `dsp_id` but it's a generated index, not a stable partner identifier.

Need:
- Prometheus metrics with `partner_id` label (instead of `dsp-{idx}`): bid rate, response latency p50/p95/p99, error rate by class (timeout, 5xx, malformed), win rate, average clearing price.
- Per-partner Grafana dashboard auto-provisioned from the partner registry.
- A partner-facing dashboard (subset of metrics, scoped to their own data) so partners can see their own performance without an account manager email loop.
- Per-partner traffic shaping: respect the `max_qps` from the registry; circuit-break a partner that returns 5xx > N% for M minutes.

#### 4. Bid request payload completeness + OpenRTB compliance

Today: BidRequest covers core fields but a real partner expects strict adherence to a specific OpenRTB version. Some examples we may not yet send:

- `source.fd` (final fan-out destination)
- `source.tid` (transaction id, distinct from our trace_id)
- `app` object for in-app inventory (we send `site` only)
- `regs.gpp` (newer privacy signal beyond GDPR/CCPA)
- `imp[].secure` (whether the page is HTTPS — required for safe iframe rendering)
- `imp[].metric[]` (publisher-supplied viewability prediction)

Need: per-partner OpenRTB version + extension support recorded in the registry. Validation harness that submits a known-good bid request to each partner's sandbox and verifies the response shape. CI test that runs against a recorded "golden" bid request to catch regressions in our payload generation.

#### 5. Bidder-specific quirks

Real DSPs have idiosyncrasies — non-standard header requirements, malformed JSON we have to tolerate, custom macro syntax in their bid responses, region-specific endpoint URLs.

Need: per-partner *adapter* in `pkg/dsp-client/adapters/`. Each adapter implements a small interface:

```go
type Adapter interface {
    PrepareRequest(*openrtb.BidRequest) (*openrtb.BidRequest, error)  // partner-specific tweaks
    ParseResponse([]byte) (*openrtb.BidResponse, error)               // tolerate quirks
    SignRequest(*http.Request) error                                  // partner-specific auth
}
```

Default adapter handles spec-compliant partners. Override per-partner only when needed. Pattern stolen from Prebid Server's adapter model — well-understood in ad tech.

#### 6. Financial integration

Today: in-memory ledger, single tenant model.

Need (most of this is already in the persistence-strategy roadmap, but per-partner adds requirements):
- Ledger entries scoped by partner_id (which DSP did this spend come from)
- Per-partner invoice generation on configurable cadence (net-30, net-60, prepay)
- Reconciliation workflow: monthly export of our spend data per partner, partner sends theirs, automated diff with tolerance (e.g. ±0.5%), surfaced discrepancies for resolution
- Dispute resolution queue in the partner portal
- Multi-currency support if partner bills in non-USD (`exchange_rates` table, daily ECB rate import)

#### 7. Privacy and compliance per partner

Today: privacy signals are stubs (the consent strings are passed but not enforced; opt-out is wired but not gated).

Need:
- Per-partner IAB TCF vendor ID stored in registry. When a user's consent string excludes that vendor, the exchange must not include their bid request in that partner's fan-out.
- GPP signal propagation (newer than TCF, region-aware)
- Audit log of "what data we sent to which partner per bid request" — required for GDPR access requests (a user can ask "tell me everywhere my data went," we must answer per partner per bid)
- Per-region data sovereignty: if partner isn't certified for EU, don't send EU traffic
- Right-to-deletion propagation: when a user requests deletion, hit each partner's deletion endpoint and verify success (some partners support this via OpenRTB `regs.deletion_request`, others need direct API calls)

#### 8. Operational lifecycle

The non-code surface that makes partnerships work:

- Partner onboarding doc + sandbox environment (separate K8s namespace, fake traffic, no real billing)
- Partner offboarding: pause status → 30-day grace period for in-flight invoices → terminate (purge auth secrets, archive ledger, retain audit logs)
- Status page / incident comms (when our exchange has an outage, partners need to know)
- Change management: schema changes to OpenRTB payload have a notice period so partners can adapt their parsers

### Build order

1. **DSP registry table + warm cache** — unblocks everything else, small change to exchange (read from cache instead of env config)
2. **Auth-aware HTTP client + secret store integration** — required before any real partner can be added
3. **Per-partner Prometheus metrics + Grafana** — partners require SLA visibility
4. **Partner portal admin UI** — until this exists, onboarding is a manual SQL insert
5. **Adapter framework** — needed first time we onboard a partner with quirks
6. **Privacy enforcement (TCF vendor gating)** — required for any EU traffic
7. **Financial reconciliation workflow** — needed by first month-end of real spend
8. **Compliance audit log** — needed first time a GDPR access request lands

Until #1-3 are done, the exchange remains internal-only. Each subsequent step expands the partner-classes we can serve (sandbox-only → low-trust partners → regulated regions → enterprise tier-1 DSPs).

### What we *don't* build (out of scope)

- **Proprietary internal RPC protocols for internal DSPs.** Stay on OpenRTB HTTP for everyone — see "Three transports, three roles" in NATS Subjects section.
- **Bidder-side SDK.** We're the exchange, not a DSP — partners build their own bid handlers in whatever language they want, the only contract is the OpenRTB wire format.
- **Real-time settlement on every bid.** Reconciliation is monthly. Real-time spend tracking is for budget caps (Redis counter), not for invoicing.

---

## Exchange Fan-Out: Latency, Timeouts, and Adaptive Routing

How the exchange decides who to ask, how long to wait, and when to stop waiting. Tightly coupled to the External DSP Partners work above — once we onboard partners with varying latency profiles, the fan-out has to adapt.

### How it works today

`cmd/exchange/main.go:fanOutToDSPs` issues all DSP HTTP calls in parallel (one goroutine per endpoint), gathers responses through a buffered channel, and **early-finishes** when either all DSPs have reported OR the fan-out context's deadline elapses.

```
auction handler:
    fanCtx, cancel = context.WithTimeout(ctx, bid_timeout)   ← explicit deadline
    spawn N goroutines (one per selected DSP)
        each: POST /v1/openrtb/bid via http.NewRequestWithContext(fanCtx)
        each: write dspResult{...} to channel
    main loop:
        select {
            case result := <-ch:   record + accumulate bids
            case <-fanCtx.Done():  early-finish — stop waiting, drop late goroutines
        }
    run auction with whatever bids arrived
```

Three layers of "drop slow DSPs":

1. **Per-call HTTP timeout (belt-and-braces).** `httpClient.Timeout = bid_timeout` at boot. Each `client.Do` call gives up at `bid_timeout` regardless of context propagation working. Live-config aware via `sc.Manager.OnChange("exchange.bid_timeout", ...)`.

2. **Per-auction context deadline (primary).** Every outbound request uses `fanCtx` which has `WithTimeout(ctx, bid_timeout)`. When the deadline elapses, in-flight `client.Do` calls return a context-cancelled error AND the main loop exits via `<-fanCtx.Done()`. Auction worst-case latency = `bid_timeout` exactly — late goroutines write to the buffered channel and are simply not read.

3. **Across-auction adaptive skip (learned).** `router.RecordCall(channel, endpoint, bidReceived, topBid, latency, timedOut)` feeds `pkg/optimise.SmartRouter` after every fan-out. Stats are keyed per `(channel, dspID)` so a DSP that's great at display isn't penalised by poor video performance. Every threshold is LIVE config (staff portal, `exchange.routing_*`): DSPs whose bid rate falls below `routing_min_bid_rate` (default 5%) or timeout rate exceeds `routing_max_timeout_rate` (default 50%) after `routing_min_calls` samples (default 20) get filtered out of future fan-outs. Permanent blackballing is prevented by the ε-probe (`routing_explore_pct`, default 1% of auctions still call a skipped DSP, deterministic per trace) so a recovered DSP earns its way back in; `routing_never_skip` exempts deal-holding DSPs entirely, and `routing_enabled=false` is the kill-switch. At N replicas, per-pod stats converge to the cluster-global `dsp_calls` aggregate via a periodic reseed from reporting (`routing_reseed_interval`), and the debug reset broadcasts on `adtech.cache.invalidate.router-stats` so it wipes every pod.

### Build status

| Gap (originally documented) | Status |
|---|---|
| Explicit `bid_timeout` deadline on the fan-out context (not just inherited request timeout) | ✅ Done. `bidTimeoutFn` closure reads live config per request; `context.WithTimeout` derives `fanCtx`. |
| Explicit `http.Client.Timeout` as belt-and-braces | ✅ Already in place before this work — `httpClient := &http.Client{Timeout: bidTimeout}` at boot. |
| Main loop waits for every goroutine even after the auction would have completed | ✅ Done. Gather loop now `select`s on `<-ch` or `<-ctx.Done()`. Auctions complete at exactly `bid_timeout` instead of "slowest DSP's actual response time." |
| SmartRouter EV scoring (`bid_rate × avg_bid × win_rate`) | ✅ Already in place — preserved during the per-channel refactor. |
| SmartRouter per-channel routing | ✅ Done. Stats keyed by `(channel, dspID)`; `SelectDSPs(channel, ...)` and `RecordCall(channel, ...)` carry channel through. New unit test `TestSmartRouter_PerChannelStats` proves channel isolation. |
| All DSPs share one `bid_timeout` value | ⏳ Pending. Requires External DSP Partners step 1 (registry table) — per-partner `timeout_ms` lives in `partners` row, fanCtx becomes per-DSP. |
| SmartRouter evaluation-status quota | ⏳ Pending. Requires partner registry (need "status: evaluation" metadata on each partner). |

### Implementation references (for future readers)

- Fan-out function: `cmd/exchange/main.go:fanOutToDSPs` — parallel goroutines + early-finish select
- Timeout wiring: `cmd/exchange/main.go` — `bidTimeoutFn` closure passed to `auctionHandler`; `fanCtx, fanCancel := context.WithTimeout(ctx, bidTimeout)`
- SmartRouter: `pkg/optimise/routing.go` — `(channel, dspID)` keyed stats, EV scoring with latency penalty
- Tests: `pkg/optimise/optimise_test.go` (`TestSmartRouter_Selection`, `TestSmartRouter_PerChannelStats`); e2e `TestSmartRoutingTracksDSPStats` and `TestBidShadingTrackerRecords` continue to pass

### Deferred / opt-in: extended-timeout fallback

A "two-tier timeout" — wait short by default, extend the deadline if zero bids arrived — was discussed and **explicitly not built**. Reasons:

- Not standard OpenRTB practice. The protocol defines TMax as a hard deadline; publishers set it based on their page-render budget, not based on "we'd like a bid eventually."
- Predictable latency matters more than incremental bid recovery. Publishers prefer "auctions complete in ≤100ms always" over "≤100ms usually, ≤200ms sometimes" — easier to size their render pipeline.
- The right fix for "too many no-bids" is supply-side (more bidders, better routing), not "wait longer."
- Standard fallback for empty auctions is a **house ad** at the ad-server level, not extending the auction. That keeps the auction itself fast and decouples the empty-case handling.

If this becomes useful for experimentation later, the design is: add `exchange.bid_timeout_extended` config (default 0 = disabled). When set and the primary deadline elapsed with zero bids, switch the `<-ctx.Done()` branch to "wait until extended deadline OR ≥1 bid." Off by default, on for measurement runs. Not on the active build list.

### Why this matters more once we have external partners

With only internal DSPs, all latencies are tight (~1-5ms in-cluster) and uniform — the original "wait for all" loop cost almost nothing. With external partners on the wire, response times span 20-150ms and vary by partner/region/load. The early-finish + per-channel routing + explicit deadline combination prevents one slow partner from gating the auction or muting a channel-specialist's selection.

Remaining work to do **before** onboarding the first external DSP: the per-partner-timeout (build status row above) — landed via the partner registry from External DSP Partners section.

---

## Identity and First-Party Data

### Identity Layers

| Layer | Source | Reliability | Example |
|---|---|---|---|
| **Platform ID** | First-party cookie set by SSP ad tag | High (our cookie, our domain) | `adtech_uid = "uuid-123"` |
| **Publisher authenticated ID** | Publisher's logged-in user data | Highest (deterministic) | Publisher passes their user_id, hashed email |
| **Advertiser first-party data** | CRM uploads, customer lists | Highest (deterministic) | Hashed email lists, purchase segments |
| **Deterministic match** | Hashed email/phone matching across parties | High | SHA256(email) matches publisher + advertiser data |
| **Probabilistic signals** | IP + user agent + screen + behaviour | Low-medium (fallback) | Likely same device/user |

### Platform ID (Our First-Party Cookie)

```
User visits publisher page with our ad tag
    |
    v
First visit?
    +-- Yes -> SSP generates UUID, sets first-party cookie: adtech_uid = "uuid-123"
    +-- No  -> SSP reads existing cookie
    |
    v
Platform ID flows through the entire chain:
    SSP bid request -> Exchange -> DSP -> Ad Server -> Tracker -> Reporting
    |
    v
Used for: frequency capping, basic retargeting, conversion attribution
```

Since we own the full stack, one ID works everywhere - no cross-domain cookie syncing needed. This is a major advantage over platforms that depend on third-party cookies.

### Publisher First-Party Data Ingestion

Publishers can pass their own user data via the ad tag:

```javascript
// Publisher's ad tag (on their page)
adtech.setUserData({
    publisher_user_id: "pub_456",          // their internal user ID
    hashed_email: "sha256(john@...)",      // hashed, never raw PII
    segments: ["sports", "subscriber"],     // publisher's own segments
    age_range: "25-34",                     // demographics
    gender: "male",
    consent: {gdpr: true, ccpa: true}       // consent signals
});
```

This data is included in the bid request alongside the platform ID:

```json
{
    "user": {
        "id": "uuid-123",
        "publisher_user_id": "pub_456",
        "hashed_email": "sha256_abc",
        "segments": ["sports", "subscriber"],
        "demographics": {"age_range": "25-34", "gender": "male"},
        "consent": {"gdpr": true, "ccpa": true}
    }
}
```

The DSP uses this for targeting - a campaign targeting "sports enthusiasts aged 25-34" matches this user precisely, without guessing.

### Advertiser First-Party Data Upload

Advertisers upload their CRM data to the platform:

**Upload formats:**
- Hashed email lists (CSV of SHA256 hashes + segments)
- Customer segments (existing customers, high-value, lapsed, etc.)
- Conversion data (offline purchases matched back via hashed email)

**API:**
- `POST /v1/api/audiences/upload` - upload hashed customer list
- `GET  /v1/api/audiences` - list audience segments
- `POST /v1/api/audiences/segment` - create custom segment from rules

**How it's used in bidding:**

```
DSP evaluates bid request:
    |
    v
    Does bid_request.hashed_email match any advertiser audience?
    |
    +-- "existing_customer" segment -> bid higher (retarget)
    +-- "high_value" segment -> bid much higher
    +-- "lapsed_customer" segment -> bid with win-back creative
    +-- No match -> bid based on other signals (geo, context, publisher segments)
```

### Identity Graph

Over time, the platform builds a graph connecting signals for the same user:

```
Platform ID "uuid-123"
    |
    +-- Publisher A: publisher_user_id "pub_456", hashed_email "sha256_abc"
    +-- Publisher B: publisher_user_id "pub_789", same hashed_email "sha256_abc"
    +-- Advertiser X: matched to "high_value" segment via hashed_email
    +-- Device 1: Chrome on macOS, IP range 192.168.x.x
    +-- Device 2: Safari on iPhone, same hashed_email -> same person, cross-device
```

**Matching rules:**
- **Deterministic:** Same hashed email across publisher/advertiser = confirmed same person
- **Probabilistic:** Same IP + similar UA within short time window = likely same person (lower confidence)
- **Cross-device:** Same hashed email on different devices = same person, different device

The identity graph powers:
- Cross-publisher frequency capping (user saw ad on Publisher A, don't show again on Publisher B)
- Cross-device attribution (saw ad on mobile, converted on desktop)
- Advertiser retargeting across the publisher network

#### Real-Time Retargeting (Built) — status 2026-08

Abandoned-cart retargeting normally lags: the `/v1/t/rt` pixel publishes a
`site_visit` behaviour signal, but membership is written by the hourly batch
profile-builder — so a visit at 2:05 doesn't become targetable until ~3:00.
`cmd/audience-rt` closes that gap. It's a one-replica consumer that, on each
`site_visit`, enrolls the visitor into the advertiser's retargeting segment(s)
immediately (single-visit rules, `min_count<=1`) and broadcasts
`cache.invalidate.audience`, so the DSP retargets within **seconds**, not an
hour. On a `purchase` conversion it suppresses the buyer (removes them) so we
stop paying to chase a converted user. It writes the same
`audience_segment_members` the batch builder would — the DSP bid path is
unchanged, only the latency. Frequency-threshold rules (`min_count>1`) stay with
the batch builder. e2e: `tests/e2e/retargeting_realtime_test.go` (visit → enrolled
in seconds without running the builder → DSP retargets → purchase → suppressed).
Core: `pkg/retargeting`.

### Privacy and Consent

| Control | How it works |
|---|---|
| **Consent required** | Identity graph and first-party data only used when user has consented (consent signal in bid request) |
| **No consent** | Ads still served but untargeted - contextual only (page content, geo from IP). No identity graph, no frequency cap, no retargeting. |
| **User opt-out** | Platform ID cookie deleted. User removed from identity graph. Future visits treated as new unknown user. |
| **Right to deletion** | User requests deletion -> purge from identity graph, all audience segments, all event records linked to their ID. Propagated to all stores (Postgres, analytics, Redis freq caps). |
| **Data retention** | Platform IDs expire after 90 days of inactivity. Identity graph entries pruned on same schedule. |
| **Hashing only** | Raw PII (email, phone) is never stored. Only hashed versions (SHA256). Hashing happens client-side in the ad tag before data leaves the user's browser. |
| **Advertiser data isolation** | Advertiser A's customer list is never visible to Advertiser B or any publisher. Multi-tenancy RLS enforces this. |
| **Publisher data isolation** | Publisher A's user data is not shared with Publisher B. Cross-publisher matching only uses platform ID and deterministic signals (hashed email). |
| **Consent signal propagation** | Consent status flows through the entire chain: ad tag -> SSP -> Exchange -> DSP -> Ad Server -> Tracker. If consent is absent at any point, user-level targeting is suppressed. |

### Clean Room (Future)

A secure matching environment where publisher and advertiser data overlap can be calculated without either side seeing the other's raw data:

- Publisher uploads hashed audience to clean room
- Advertiser uploads hashed customer list to clean room
- Platform computes overlap size and match segments
- Neither side sees the other's full list
- Results: "your customer list matches 15% of Publisher X's sports audience"

Deferred - but the identity graph architecture supports it. The `pkg/audience/` package would host clean room logic when ready.

### Audience Segment Delivery (Read Path)

When a bid request flows through the platform, two distinct enrichment paths attach audience signals to it. Both are wired today (see "Current State" below); both share the same storage but read with different filters.

**Why two paths?**

In the real industry SSPs and DSPs are different companies with different data assets. The protocol assumes both can contribute signals about a user, and the architecture has to keep them cleanly separated so neither leaks to the other (or, in path B's case, to rival DSPs receiving the same fan-out).

| | Path A: SSP-public | Path B: DSP-private |
|---|---|---|
| **Who collected the membership** | Publisher / SSP DMP — site-level behaviour, publisher CRM, third-party data provider | DSP — its own pixels on advertiser sites, CRM uploads, lookalikes, suppression lists |
| **How it reaches the DSP** | SSP looks up segments on inbound request, stamps `user.ext.segments` on the outbound OpenRTB bid request | DSP looks up its own segments after receiving the bid request, before targeting evaluation |
| **Who can see it** | Every DSP that gets the bid request fan-out | Only the DSP that owns the segment |
| **Use cases** | Generic context like "sports_fan", "subscriber" — value to all bidders | Competitive edge: "browsed_rolex_yesterday", "lifetime_value_$4k+" — never leaves the DSP |
| **Encoded as** | `audience_segments.visibility = 'public'` | `audience_segments.visibility = 'dsp_private'` |

**Flow:**

```
                Postgres: audience_segments + audience_segment_members
                                       │
        ┌──────────────────────────────┼──────────────────────────────┐
        │ visibility='public'          │ visibility='dsp_private'     │
        ▼                              ▼                              ▼
   SSP lookup                     (filtered out                  DSP lookup
   SegmentsForUser                 from SSP query)              DSPSegmentsForUser
        │                                                              │
        ▼                                                              │
   user.ext.segments = [public segs]                                  │
        │                                                              │
        ▼                                                              ▼
   Exchange fan-out  ──────────────────────►  DSP bid handler        │
                                                  │                    │
                                                  ├────────────────────┘
                                                  ▼
                                  tReq.Segments = public ∪ dsp_private
                                                  │
                                                  ▼
                                  targeting.Evaluate(campaign, tReq)
```

The DSP performs a **union** rather than a replace — a campaign can match on either source, and bid modifiers stack from both.

**Current State (what's wired):**

- ✅ `audience_segments` table (migration 011) — segment definitions with `account_id`, `type`, `source`
- ✅ `audience_segment_members` table (migration 019) — user_id → segment_id edges, indexed on user_id for hot-path lookup
- ✅ `audience_segments.visibility` column (migration 020) — `public` | `dsp_private`
- ✅ `pkg/audience/store/postgres.Store` — `SegmentsForUser` (path A) and `DSPSegmentsForUser` (path B), both filter by visibility via JOIN
- ✅ SSP wiring (`cmd/ssp/main.go`) — opens store at boot; on each `/v1/ssp/request` with `user_id`, looks up public segments and stamps `user.ext.segments`. Nil-tolerant: SSP keeps serving without enrichment if Postgres is unreachable.
- ✅ DSP wiring (`cmd/dsp/main.go`) — opens store at boot; in `bidHandler`, after reading SSP-stamped segments, unions in DSP-private segments before `targeting.Evaluate`.
- ✅ Targeting evaluator (`pkg/targeting`) — already supports `include_segments` / `exclude_segments` and segment bid modifiers. Both paths exercise the same evaluator code.
- ✅ E2E tests — `TestTargetingSegmentInclude` (path A), `TestTargetingDSPPrivateSegment` (path B).

**What's NOT wired (production wishlist):**

| Gap | What to build | Why it matters |
|---|---|---|
| **Hot-path caching** | Redis HSET keyed by `user:{id}:segments`, populated from Postgres on cache miss + invalidated on membership change. Add a Bloom filter in-process to skip the Redis call entirely for users with no segments (the common case). | Today every bid request is a Postgres roundtrip per service (SSP + DSP) — fine for dev/test, melts under prod load. Warm-cache pattern (`pkg/cache/warm`) doesn't fit: the dataset is O(100M users × N segments), can't hold in process memory. |
| **Membership write pipeline** | Tracker NATS consumer applies behavioural rules (visited X pages → enroll in segment Y); CRM upload endpoint writes batches; lookalike batch job writes modelled members. All write to `audience_segment_members`. | Today the only writers are the seed and the e2e harness — direct SQL. No real-world ingestion path exists. `cmd/pipeline/` and `pkg/pipeline/` are scaffolded but not wired for this. |
| **Segment lifecycle** | TTL on memberships (e.g. behavioural segments decay after 30d), refresh on re-trigger, audit log of who added/removed users. | Without TTL, "viewed_product" segments grow forever and lose targeting value. Without audit, GDPR access-request requests can't be answered. |
| **Cross-account access controls** | SSP currently returns every public segment for a user regardless of which advertisers are bidding. Real platforms gate visibility via deals (advertiser X has a deal that includes audience Y → only their bid sees Y in `user.data`). | Today's behaviour leaks targeting opportunities across advertisers. Tightening requires linking deal IDs to segment access policies. |
| **Composite segments** | `audience.Store.EvaluateComposite` exists in-memory but Postgres path doesn't evaluate composite rules at lookup time. | Advertisers express segments as "(visited_product_page AND NOT existing_customer) OR (lookalike_to_top_buyers)". Without composite evaluation, this collapses to creating many flat segments. |
| **Identity graph integration** | Lookup keys are raw `user_id` strings today. In prod the SSP would resolve `(publisher_user_id, hashed_email, cookie_id)` to the canonical platform user ID via `pkg/identity` before the segment lookup. | A user known to advertiser X via hashed email might also be known to the SSP via publisher cookie. Without the graph, the two stay separate and segment hit-rate is artificially low. |
| **Consent gating** | SSP looks up and stamps unconditionally today. Should check `regs.gdpr` / `user.ext.consent` and skip segment stamping if consent is missing or revoked. | Privacy law compliance. Path B has the same gap on the DSP side. |
| **Suppression list optimisation** | A "do_not_target" segment with 10M user IDs queried on every bid request via the same JOIN — expensive. Should be a Bloom filter loaded into memory at the DSP, checked before the campaign loop. | Suppression lists are common (existing customers excluded from acquisition campaigns) and have very different lookup characteristics than positive targeting. |
| **Membership scale validation** | Currently no benchmarks for "1M users × 50 segments × N bid requests/sec." Need load tests before swapping any of the above into the hot path. | Cache strategy choice depends on real-world numbers (avg segments per user, % of users with any segments, segment-add rate). |

The recommended build order when this work is picked up:

1. **Membership write pipeline first** — without realistic data, none of the perf work can be benchmarked properly. Wire `cmd/pipeline/` to consume tracker events and write behavioural memberships.
2. **Redis cache with Bloom filter prefilter** — the single biggest latency win. SSP and DSP each get their own Redis client pointing at the same key namespace (or sharded by source if isolation matters).
3. **Identity graph resolution** — once the cache is in place, swap raw `user_id` lookups for canonical-ID lookups via `pkg/identity`.
4. **Consent gating** — small change, high compliance value. Goes in last because it requires consent signals to actually flow through the pipeline (`pkg/privacy` integration).
5. **Composite + suppression + lifecycle + cross-account ACLs** — incremental on top.

### Implementation

| Component | Location |
|---|---|
| Platform ID generation | SSP ad tag (JavaScript) + `cmd/ssp/` |
| Identity graph store | Postgres (graph edges) + Redis (fast lookups during bidding) |
| Identity graph builder | `pkg/identity/` - matching logic, graph operations, merging |
| Audience management (definitions) | `pkg/audience/` - segment types, in-memory store for tests, composite rule eval |
| Audience read path (Postgres) | `pkg/audience/store/postgres/` - `SegmentsForUser` (public, SSP) + `DSPSegmentsForUser` (private, DSP) |
| Audience write path | **NOT BUILT** - target: `cmd/pipeline/` consumes tracker events, writes to `audience_segment_members`. See "Audience Segment Delivery (Read Path)" → wishlist |
| Audience hot-path cache | **NOT BUILT** - target: Redis HSET + in-process Bloom filter. See wishlist for the staging order |
| Privacy controls | `pkg/privacy/` - consent checking, opt-out, deletion propagation |
| Ad tag SDK | `web/static/adtech.js` - publisher-facing JavaScript tag |
| First-party data API | Gateway: `/v1/api/audiences/*` |

### Profile Store (Normalized Signals → Expansion → Memberships → Export)

The answer to "do we have a profile store — many sources in, expand, memberships
out?" is: **we have all the organs but the profile itself is virtual**, assembled
at read time. This section documents what exists, the decision on graph
databases, and the target architecture for making the profile a real, batch-built
artifact on the existing Delta lake.

#### Current state (as-built, 2026-07)

```
Sources (ingest)                                  Storage
────────────────                                  ───────
SSP observed signals            ──NATS──►  cmd/identity-consumer  ──►  identity_graph (PG)
 (user_id, uid2, hashed_email,             (batch/dedup; deterministic
  ifa, publisher_user_id,                   co-occurrence conf 1.0 +
  household)                                probabilistic IP+UA opt-in)
Gateway POST /v1/api/identity-links ─────────────────────────────►  identity_graph (PG)
 (UID2-centric or generic edges — the CRM-match path)
Seed / harness / gateway audiences API ──────────────────────────►  audience_segment_members (PG)

Expansion (read time)
─────────────────────
DSP  (dsp.identity_resolution_enabled): in-memory graph preload, BFS with
     IdentityMaxDepth + IdentityMinConfidence → union DSPSegmentsForUser
     across the resolved cluster (cmd/dsp/identity.go)
SSP  SegmentsForUser on user key + household id (no graph walk)
```

The **effective profile** of a user = `resolve(any identifier)` → cluster of
device_ids / uid2 / hashed_emails / household → union of memberships across the
cluster. Nothing materializes that; it's recomputed per lookup. Consequences:
no place to hang non-segment attributes (declared demographics, derived
interests, recency/frequency counters), no reprocessing story (change a rule,
can't rebuild memberships from history), and the SSP side never expands at all.

#### Decision: no dedicated graph database

Considered: Neo4j / Memgraph / dgraph as the identity-graph store. **Rejected.**

| Consideration | Verdict |
|---|---|
| Query shape | Shallow BFS (1–3 hops) + offline clustering. Not deep traversals, not pathfinding. A recursive CTE or an in-memory adjacency walk covers it — and the DSP preload resolver already does. |
| Scale | The whole graph fits in one pod's memory (proven by the preload resolver). Graph DBs earn their keep at billions of edges with online mutation + traversal; we batch. |
| Industry pattern | Real identity vendors (LiveRamp-style) don't serve from a graph DB either — they run **batch clustering over a data lake** and serve materialized clusters from a KV store. The graph DB is the wrong layer. |
| Stack ethos | Go everywhere, one code path, minimal services. A JVM graph DB + query language for one table is the opposite. |

**So:** `identity_graph` in Postgres stays the system of record for edges
(write path unchanged). The upgrade that matters is not a better graph store —
it's **materializing the clusters** so expansion happens at write time, not
per-bid.

#### Target architecture: lake-based profile store (Delta tables on Minio)

The normalized store is **Delta tables on Minio** — not more Postgres. We
already run this exact pattern (pkg/store/datalake, the hot/cold analytics
store, DuckDB-over-Delta reads), so it's zero new infra. The lake is
append-only truth for signals; Postgres/Redis stay the serving layer; the lake
is **never on the bid path**.

```
WRITE (ingest → normalize)                          lake = Delta on Minio
──────────────────────────
SSP observed signals ──NATS──► identity-consumer ──► identity_graph (PG, edges)
tracker events (impr/click/conv) ──► pipeline ─────► lake: events        (exists)
CRM uploads (gateway audiences API) ───────────────► lake: profile_signals (new)
bucket drop-zone (files landed in Minio: CSV/Parquet
 audience files from advertisers/partners; pipeline
 detects, validates, normalizes — pkg/pipeline) ───► lake: profile_signals (new)
publisher 1P / CDP connectors (future) ────────────► lake: profile_signals

EXPAND (batch — cmd/profile-builder, DuckDB over Delta + PG)
────────────────────────────────────────────────────────────
1. Identity resolution: connected components over identity_graph edges
   (min-confidence threshold, hop cap, household edges kept as their own
   grouping level) → lake: identity_clusters (person_id → member ids)
   + slim PG copy for serving lookups
2. Segmentation: behavioural rules + composite/lookalike evaluation over
   lake events ⋈ profile_signals ⋈ identity_clusters. Enrollment is at
   person level, then **expanded to every id in the cluster** on write
3. Materialize: write audience_segment_members (PG) + warm the Redis
   hot-path cache

EXPORT / SERVE
──────────────
SSP/DSP bid path: membership lookup (PG/Redis) — unchanged shape, now hits
   pre-expanded rows; DSP read-time BFS stays as the freshness top-up
Exports: per-account Parquet/CSV segment exports to Minio (external delivery)
GDPR: access requests + deletion audits answered from the lake in one place
Profile API: GET /v1/api/profiles/{id} (staff) — cluster + signals +
   memberships, trace-explorer-grade transparency
```

**Two expansion strategies, deliberately both:**

| | Read-time BFS (built, DSP) | Write-time clustering (to build) |
|---|---|---|
| Freshness | Live — sees edges from seconds ago | Stale up to one batch interval |
| Bid-path cost | BFS walk per request (in-memory, cheap but real) | Zero — plain key lookup on pre-expanded rows |
| Coverage | Only where enabled (DSP) | Everywhere, including SSP stamping |
| Role going forward | Freshness top-up / private-edge nuance | The default expansion mechanism |

(The graph **is** the expansion mechanism in both — the alternative of
promoting one deterministic key (hashed_email/UID2) to be *the* join key was
rejected: it silently drops device-only and probabilistic-only users, and the
graph subsumes it anyway.)

**Why the lake and not Postgres for the normalized store:** reprocessing
(change a behavioural rule → rebuild all memberships from historical events —
impossible once you've only kept the latest PG rows), signal-history volume
(O(events), not O(users)), schema evolution + time travel via Delta Log, and
one place to answer "what do we know about this user" for both the profile API
and GDPR. Serving stays PG/Redis because the bid path needs single-digit-ms
lookups, which a lake never gives you.

**Why not everything in Delta (considered, rejected for now):** moving the
graph itself to the lake doesn't remove a store — the bid path needs point
lookups, so a PG/Redis serving copy must exist regardless, and PG is already
paid for (campaigns, accounts, memberships). Meanwhile the lake makes the
graph's hardest requirements worse: (a) the identity-consumer writes small
batches every ~10s — tiny Delta commits mean small-file sprawl + a compaction
job just to stay readable; (b) no unique constraints — edge dedup is one
upsert in PG, a MERGE/rewrite downstream in Delta (and `pkg/store/datalake` is
an append+log writer, not a MERGE engine); (c) GDPR level-3 deletion is a
transactional DELETE today with immediate `privacy-verify` residual checks —
in Delta it's a file-rewrite batch with hours-later consistency, on the most
privacy-sensitive table in the platform; (d) freshness — an observed edge is
queryable seconds later via PG, vs waiting for the next batch append + cluster
run. **The dividing rule: the lake holds what must be replayable (raw
observation log → `profile_signals`); Postgres holds what must be
point-readable and instantly deletable (current deduped edges + materialized
clusters).** At LiveRamp scale the system of record flips to the lake — the
escape hatch is designed in: point profile-builder's input at a lake edge
table and shrink PG to the serving tables; nothing on the bid path changes.

**Pod-direct snapshot loading (open option for `identity_clusters`):** for
*batch-built, read-only* artifacts, PG is arguably just a loading dock — the
DSP resolver already consumes the graph as a whole in-memory snapshot, with PG
as the pickup point. Since `identity_clusters` is rebuilt wholesale each run,
pods could instead load the Delta snapshot **directly from Minio** (Delta log
= atomic versioned publishes + rollback via time travel; `pkg/store/datalake`
already reads this) and poll for new versions — skipping the PG hop entirely.
Trade-offs vs the PG serving copy: (+) one less copy step, natural versioning;
(−) Minio becomes a serving-pod boot dependency (needs the same
degrade-gracefully story warm caches have for PG-down), and GDPR deletes baked
into a snapshot persist until the next build unless you add tombstone/forced-
refresh machinery (PG + NATS invalidate purges in seconds today). **DECIDED
(Phase 4, 2026-07-15): PG serving copy.** The profile-builder rebuilds
`identity_clusters` wholesale in one PG transaction per run; GDPR deletes
propagate at PG speed rather than waiting out a baked snapshot; serving pods
gain no Minio boot dependency; and the table stays small (multi-member
clusters only — singletons aren't materialized). The Delta artifact the
builder also publishes each run keeps this reversible: pod-direct snapshot
loading remains the documented escape hatch at scale. This option was for
clusters only — edges need upserts,
memberships need interactive writes + point lookups at scale + instant
deletes; those stay PG/Redis either way.

#### Implementation plan (grounded in code, investigated 2026-07-15)

**Machinery that already exists (verified seams — reuse, don't rebuild):**

| Piece | Where | State |
|---|---|---|
| Delta lake writer (Parquet + Delta commit, Snapshot, Compact; single-writer) | `pkg/store/datalake/objstore.go:93-367` | ✅ production-grade; no partitioning yet |
| Always-on lake ingest: NATS → buffered batches → Delta tables in `adtech-datalake` (ack-after-flush, 500 rows / 15s) | `cmd/pipeline/datalake_sink.go` (7 event tables) | ✅ — `profile_signals` is "one more table schema + a new source", not new machinery |
| Audience upload API (JWT-bound account, UpsertSegment + idempotent AddMembers + `adtech.cache.invalidate.audience`) | `cmd/gateway/audiences.go:47-147` | ✅ JSON body only — no file upload, no lake copy, no match rate |
| Serving loaders: Redis read-through (`audience:user:{id}:{visibility}`) + interval bulk-preloader (negative caching, TTL 3×interval) | `pkg/audience/store/cached`, `pkg/audience/store/preload` | ✅ SSP wires preload today — **the "load into auctions" story mostly exists** |
| DSP read-time graph expansion (in-mem preload, BFS depth/conf caps, 25ms budget) | `cmd/dsp/identity.go` | ✅ behind `dsp.identity_resolution_enabled` |
| Batch-job patterns to copy | CronJob `cmd/dayboundary`; replace-by-window rollups `cmd/reporting/rollup.go`; SKIP-LOCKED queue `pkg/reportjobs` | ✅ pick per job |
| GDPR purge + verify (identity_graph, audience_segment_members) | `pkg/privacydelete` | ✅ — new stores must register in its Systems |

**Two discoveries this plan must fix:**

1. **Analytics events carry no user key and no page context — behavioural
   segmentation cannot be computed from today's lake.** impressions / clicks /
   conversions / views have NO `user_id` and NO category/page URL (only
   `placement_id`) — deliberate, it's why privacy-delete never touches
   analytics. Do NOT add identity to the money tables; Phase 2 adds a
   dedicated consent-gated `behaviour_signals` table instead.
2. **Audience bid modifiers are dead code.** The DSP builds `ModifierContext`
   without `Segments` (`cmd/dsp/main.go` ~977), so `Modifiers.Audience` never
   applies to any bid. Fix folded into Phase 4.

**Phase 1 — Onboarding (first-party + third-party) → `profile_signals`**

- **Portal UI (advertiser + publisher):** an Audiences page — segment list
  (`ListSegments` exists), **CSV file upload for small files** (≤ ~5 MB,
  multipart → gateway; hash PII client-side before transmission per privacy
  rules), visibility picker (public / dsp_private), member counts, and the
  **match rate** per upload (fraction of uploaded ids resolvable via
  `identity_graph` — that number is the onboarding product). Larger files go
  via the drop-zone, and the UI says so.
- **Gateway:** extend `/v1/api/audiences` with a multipart CSV variant
  (JWT-bound account as today); compute + persist match rate on the segment.
- **Third-party drop-zone:** new `adtech-onboarding` bucket,
  `{provider}/incoming/` prefix, per-provider manifest + schema contract (id
  types delivered, consent basis, licence, access = `purchased:`/`barter:`).
  `cmd/pipeline` gains a bucket **poller** (List on interval — Minio S3-event
  support isn't assumed) reusing `pkg/pipeline` CSV validate/normalize;
  rejected rows persist to `{provider}/rejected/` (today's quarantine is
  in-memory only — fix that here).
- **Both paths:** append normalized rows to the **`profile_signals`** Delta
  table (id_type, id_value, source, access, account_id, provider, attributes,
  consent, observed_at) AND write PG memberships as today. The lake copy is
  what makes memberships recomputable.

**Phase 2 — Behavioural signal capture (`behaviour_signals` lake table)**

- New NATS subject (e.g. `adtech.behaviour.observed`), published **only when
  consent permits personalisation** (`pkg/privacy` decision): SSP emits
  request-level rows (user key, household, placement, publisher, channel,
  **content category stamped at event time** from its placement warm cache —
  rows must be self-contained); tracker emits interaction rows (impression /
  click / conversion / view with campaign + creative).
- `cmd/pipeline` sinks it to `behaviour_signals` exactly like the 7 existing
  tables.
- **GDPR:** these lake rows carry user keys, so build the lake purge —
  filtered file rewrite (reuse the Compact machinery) — and register both
  lake tables in `pkg/privacydelete` Systems + Residual. Close the
  `freq_cap_blocks` purge gap (it has user_id and isn't purged today) in the
  same change.

**Phase 3 — `cmd/profile-builder` (the expansion engine)**

- New one-shot binary on a k8s CronJob (dayboundary pattern), hourly to start.
  Three jobs per run, replace-by-window idempotency like rollups:
  1. **Clustering:** `LoadIdentityGraph` (exists, interned adjacency) →
     union-find connected components (min-confidence, hop cap; households as a
     sub-grouping) → `identity_clusters` Delta artifact + slim PG serving copy.
  2. **Behavioural rules:** rule definition lives on the segment row (add
     `rule JSONB` to `audience_segments`); evaluate via DuckDB over
     `behaviour_signals` (fallback join: events → placements → categories);
     enroll at person level → **expand to every member id in the cluster** →
     `AddMembers`, prune members that no longer qualify (replace-by-segment
     semantics), publish `adtech.cache.invalidate.audience`.
  3. **Reconcile:** replay `profile_signals` rows not yet reflected in PG
     memberships (crash/replay safety).
- Composite + lookalike rules: later, same seam.

**Phase 4 — Serving: "loading it into the auctions" (last mile only)**

- Because memberships are **pre-expanded at write time**, the bid path needs
  no graph walk: the existing preloader/Redis read-through already delivers
  them to SSP stamping and DSP private union unchanged. Keep DSP read-time
  BFS as the freshness top-up for edges observed since the last batch.
- **Fix the dead audience modifiers** (populate `modCtx.Segments` in the DSP
  bid handler) so segment bid modifiers actually price bids.
- `identity_clusters` serving copy: **decided — PG** (see the decision note
  above). Needed only for the profile API and optional SSP person-level
  stamping, not the hot path; the builder already writes it (Phase 3).

**Phase 5 — Payoff valves**

- **Exports:** per-account segment export as Parquet/CSV to Minio (reuse the
  `reportjobs` queue + artifact pattern).
- **Profile API:** `GET /v1/api/profiles/{id}` (staff-scoped) — cluster
  members, signal summary, memberships with provenance; portal page in the
  trace-explorer style.
- **Staff onboarding monitor:** drop-zone job status, rejected-row counts,
  per-provider match rates.

**E2E gates:** upload → match rate reported → segments targetable in auction;
drop-zone file → memberships → auction; behavioural rule flips a targeting
decision; level-3 deletion purges PG + both lake tables (verifier green);
audience bid modifier changes a winning price.

Update `docs/diagrams/` (identity/data-flow D2) in the Phase 3 PR — that's
where service connections change (new binary, new bucket, new subject).

---

## Platform Workflows

### Content Moderation

Platform-side review queues:

| Queue | What's reviewed | Who reviews |
|---|---|---|
| Creative review | New/updated creatives flagged by auto-scan | Platform ad ops |
| New advertiser review | First-time advertisers before campaigns go live | Platform account team |
| New publisher review | New publishers before inventory goes live | Platform account team |
| Reported ads | Ads reported by publishers or end users | Platform ad ops |
| Credit approval | Advertisers requesting credit terms (non-prepay) | Platform finance |

Dashboard views:
- `GET /v1/api/moderation/queue` - pending items by type
- `POST /v1/api/moderation/{type}/{id}/approve` - approve item
- `POST /v1/api/moderation/{type}/{id}/reject` - reject with reason

### API Access Tiers

| Tier | Rate limit | Who |
|---|---|---|
| Dashboard (JWT session) | 100 req/sec per user | Human users in browser |
| API key (standard) | 500 req/sec | Small-medium publishers/advertisers |
| API key (enterprise) | 5000 req/sec | Large publishers, agencies, in-house teams |

API key tier is set per account. Configurable via live config.

---

## Ad Tech Core Features (Expanded)

### 1. First-Price Auction (Default)

The industry has moved to first-price auctions. First-price is our default; second-price available as a configuration option per deal.

**How each auction type works:**

```
First-price example:                    Second-price example:
  DSP A bids: $5.00                       DSP A bids: $5.00
  DSP B bids: $3.00                       DSP B bids: $3.00
  DSP C bids: $2.00                       DSP C bids: $2.00

  Winner: DSP A                           Winner: DSP A
  Pays: $5.00 (their bid)                 Pays: $3.01 (second + $0.01)
```

**Auction type per deal:**

| Deal type | Auction type | Price determination |
|---|---|---|
| Open auction | First-price (default) | Winner pays their bid |
| PMP | First-price (default) or second-price (deal config) | Depends on deal config |
| PG | No auction | Fixed price agreed in deal |
| Preferred deal | No auction | Fixed price, first-look then pass to open auction |

**Impact on the platform:**

| Concern | First-price impact |
|---|---|
| DSP bid logic | Must use bid shading - bidding raw value means overpaying |
| Exchange | Simpler - winner pays their bid, no second-price calculation needed |
| Billing | `AuctionWinEvent.clearing_price` = winning bid (not second price) |
| Loss notifications | Critical - DSPs need the winning price to calibrate shading |
| Publisher revenue | Generally higher - no second-price discount |
| Transparency | Advertisers see exactly what they paid = what they bid (after shading) |

**Unified Auction Engine (`pkg/auction/`):**

The Exchange supports multiple auction strategies through a pluggable design. Each ad channel uses a different strategy, but they all go through the same pipeline:

```
Bid request arrives at Exchange
    |
    v
1. DEAL CHECK: PG or Preferred deal? -> skip auction, serve deal ad
    |
    v
2. SELECT STRATEGY based on imp type:
    imp.banner     -> SingleWinnerStrategy
    imp.native     -> SingleWinnerStrategy
    imp.video      -> SingleWinnerStrategy OR PodStrategy (if ad break)
    imp.audio      -> SingleWinnerStrategy OR PodStrategy (if ad break)
    imp.ext.dooh   -> TimeSlotStrategy
    imp.ext.retail -> RelevanceWeightedStrategy
    imp.ext.game   -> SingleWinnerStrategy OR BatchStrategy (if intrinsic billboards)
    |
    v
3. COMMON PIPELINE (all strategies):
    a. Filter: remove bids below floor price
    b. Filter: remove bids blocked by publisher quality controls
    c. Filter: remove bids that violate competitive separation
    d. Filter: remove bids for creatives that don't match format/duration requirements
    e. Apply strategy-specific scoring and selection
    |
    v
4. RESULT:
    Publish AuctionWinEvent(s) (one per winner)
    Send win notices
    Send loss notices with reason
```

#### Strategy 1: SingleWinner (Display, Native, Video Single, Rewarded, Interstitial)

The simplest and most common. One placement, one winner.

```go
type SingleWinnerStrategy struct {
    PriceMode string // "first_price" or "second_price"
}

func (s *SingleWinnerStrategy) Select(bids []Bid) AuctionResult {
    sort by bid.Price descending
    winner = bids[0]
    if s.PriceMode == "first_price":
        clearing_price = winner.Price
    else:
        clearing_price = bids[1].Price + 0.01 (or floor if only one bid)
    return AuctionResult{Winners: [winner], ClearingPrice: clearing_price}
}
```

#### Strategy 2: Pod (Video Pod, Audio Pod)

Multiple winners to fill a time duration. Bin-packing with constraints.

```go
type PodStrategy struct {
    MaxDuration      int      // total break duration in seconds
    MaxAds           int      // max ads in pod
    BumperInDuration int      // bumper before ads
    BumperOutDuration int     // bumper after ads
}

func (s *PodStrategy) Select(bids []Bid) AuctionResult {
    available_duration = s.MaxDuration - s.BumperInDuration - s.BumperOutDuration

    // Sort by CPM (normalise different durations to comparable value)
    sort by bid.Price / bid.Duration * 1000 descending

    winners = []
    remaining = available_duration
    seen_advertisers = set{}
    seen_categories = set{}

    for bid in sorted_bids:
        if bid.Duration > remaining: skip
        if bid.AdvertiserID in seen_advertisers: skip (self-separation)
        if bid.Category in seen_categories: skip (competitive separation)

        winners.append(bid)
        remaining -= bid.Duration
        seen_advertisers.add(bid.AdvertiserID)
        seen_categories.add(bid.Category)

        if len(winners) >= s.MaxAds: break
        if remaining <= 0: break

    // Handle short fill
    if remaining > 0:
        apply publisher short_fill_policy (slate, early_return, re_auction)

    return AuctionResult{Winners: winners, ShortFill: remaining}
}
```

#### Strategy 3: RelevanceWeighted (Retail Sponsored Products)

Winner isn't just the highest bid. It's **relevance * bid**. A highly relevant product with a lower bid can beat an irrelevant product with a higher bid.

```go
type RelevanceWeightedStrategy struct {
    MaxWinners       int     // top N sponsored positions
    MinRelevance     float64 // minimum relevance score to be eligible
    OrganicRatio     float64 // minimum % of results that must be organic
}

func (s *RelevanceWeightedStrategy) Select(bids []Bid, query string) AuctionResult {
    // Score each bid: relevance to search query * bid price
    for bid in bids:
        bid.RelevanceScore = calculateRelevance(bid.Product, query)
        bid.AuctionScore = bid.RelevanceScore * bid.Price
        if bid.RelevanceScore < s.MinRelevance:
            bid.Eligible = false  // too irrelevant, even at high bid

    // Sort by auction score (relevance * price)
    eligible = filter(bids, bid.Eligible)
    sort eligible by bid.AuctionScore descending

    // Take top N
    winners = eligible[:min(s.MaxWinners, len(eligible))]

    // Each winner pays their bid (first-price) but ranked by score
    for i, winner in winners:
        winner.Position = i + 1  // position 1, 2, 3...
        winner.ClearingPrice = winner.Price  // first-price

    return AuctionResult{Winners: winners, Positions: true}
}
```

**Why relevance matters:**

```
Search: "running shoes"

Without relevance weighting (pure price):
    Position 1: "Car Insurance" bid $5.00 (irrelevant, bad user experience)
    Position 2: "Running Shoes A" bid $3.00

With relevance weighting:
    "Car Insurance" relevance: 0.05 -> score: 0.05 * $5.00 = $0.25
    "Running Shoes A" relevance: 0.90 -> score: 0.90 * $3.00 = $2.70

    Position 1: "Running Shoes A" (relevant, good user experience)
    -> Better for shoppers, better for retailer, better long-term for platform
```

##### Built (MVP) — status 2026-08

The relevance-weighted auction ships and is e2e-proven (`tests/e2e/retail_test.go`,
`TestRetailRelevanceBeatsHigherBid`): a relevant, cheaper product wins the sponsored
slot over an off-category product bidding ~3× more, live through SSP → DSP → exchange.

| Concern | What ships | Where |
|---|---|---|
| Strategy | `RelevanceWeightedStrategy.Select` ranks by relevance × bid, returns the top `SlotCount` as multi-winners (positions 1..N), first-price per slot; extras → outranked losses; `ShortFill` = unfilled slots. | `pkg/auction/relevance.go` (+ `relevance_test.go`) |
| Relevance | Explicit `Bid.Relevance` (0..1) when supplied, else derived from `Bid.Category` vs the request's `RetailCategories` (browsed categories): match = 1.0, miss = 0.1×, no signal → pure price. | `retailRelevance()` |
| Product SLATE | For `imp.ext.channel=retail` the DSP returns ALL eligible products (grouped by advertiser seat), not a single best bid, so the exchange can rank across the slate. Product category rides `BidObj.Cat` from the campaign's `include_categories`. | `cmd/dsp` retail-slate path |
| Soft category | On retail the DSP drops category as a HARD targeting filter (a shoe ad stays eligible on a different-category page, it just ranks lower); geo/device/audience still gate. | `cmd/dsp` (`tRules` category strip) |
| Serve | SSP `channel=retail` builds a banner-shaped sponsored-product imp (`imp.ext.channel=retail`, `?cat=` → `Site.Cat`); exchange routes retail → relevance_weighted, feeds `Site.Cat` as `RetailCategories`. | `cmd/ssp`, `cmd/exchange` |

**Multi-slot billing SHIPPED** (2026-08): `imp.ext.surfaces` (placement slot count)
→ exchange `SlotCount` → the top-N ranked products return as multi-winners, each on
its own sub-trace so every slot bills independently (see in-game per-surface billing
for the mechanism). Still deferred: rendering the sponsored-results grid UI +
per-position CLICK tracking; `MinRelevance` eligibility floor, `OrganicRatio`,
generalized-second-price pricing
(each product pays the minimum to hold its rank, as in search ads), and a proper
per-product category (distinct from `include_categories`) are also deferred.

#### Strategy 4: Batch (In-Game Intrinsic Billboards)

Multiple placements auctioned in one request with cross-placement constraints.

```go
type BatchStrategy struct {
    PlacementCount int // number of billboards to fill
}

func (s *BatchStrategy) Select(bids []Bid) AuctionResult {
    // Each bid is for "any billboard in this scene"
    // Assign bids to placements, one advertiser per placement

    sort by bid.Price descending

    winners = []
    seen_advertisers = set{}
    seen_categories = set{}

    for bid in sorted_bids:
        if len(winners) >= s.PlacementCount: break
        if bid.AdvertiserID in seen_advertisers: skip (no duplicate advertisers)
        if bid.Category in seen_categories: skip (competitive separation)

        winners.append(bid)
        seen_advertisers.add(bid.AdvertiserID)
        seen_categories.add(bid.Category)

    // Unfilled placements get default/house ads
    unfilled = s.PlacementCount - len(winners)

    return AuctionResult{Winners: winners, Unfilled: unfilled}
}
```

##### Built (MVP) — status 2026-08

The batch scene auction ships and is e2e-proven (`tests/e2e/ingame_test.go`,
`TestInGameSceneCompetitiveSeparation`): a 3-surface scene with 4 competing
products fills 3 surfaces with 3 DISTINCT advertisers — an advertiser bidding the
top TWO prices still gets only one surface.

| Concern | What ships | Where |
|---|---|---|
| Strategy | `BatchStrategy.Select` sorts by price, greedily assigns the top bids to `SlotCount` surfaces with competitive separation (one advertiser AND one category per scene), first-price per surface; blocked/overflow bids → losses; `ShortFill` = surfaces left for house ads. | `pkg/auction/batch.go` (+ `batch_test.go`) |
| Product SLATE | `imp.ext.channel=ingame` makes the DSP return ALL eligible products across advertisers (grouped by seat), so the scene has multiple advertisers to separate. Category is soft (not a hard filter) for in-game too. | `cmd/dsp` slate path (shared with retail) |
| Serve | SSP `channel=ingame` builds a banner-shaped scene imp (`imp.ext.channel=ingame`, `placement_type=intrinsic`, `?surfaces=N` → `imp.ext.surfaces`); exchange sets `Format=intrinsic` + `SlotCount=surfaces` → Batch, and returns EVERY winner (one SeatBid per advertiser) so the caller sees the whole filled scene. | `cmd/ssp`, `cmd/exchange` |

**Per-surface billing SHIPPED** (2026-08): multi-winner auctions publish one
AuctionWinEvent per surface, each on a distinct **sub-trace** (`<trace>::s<n>`, in
`cmd/exchange` `surfaceTrace`) exposed as the winning `BidObj.ID`; the renderer fires
each surface's impression on its sub-trace, so N surfaces = N billed impressions
(billing dedups on trace_id — additive, single-winner path unchanged). Still
deferred: the SSP scene render + a per-product category distinct from
`include_categories`.

#### Strategy 5: TimeSlot (DOOH Screen Rotation)

Bid for a time slot in a screen's rotation. Different from page-load auctions because the screen continuously rotates through ads.

```go
type TimeSlotStrategy struct {
    RotationDuration int   // total rotation cycle in seconds (e.g. 60s)
    SlotDuration     int   // each ad slot duration (e.g. 10s)
    SlotsPerRotation int   // e.g. 6 slots in a 60s rotation
}

func (s *TimeSlotStrategy) Select(bids []Bid) AuctionResult {
    // Fill rotation slots with highest-paying, non-competing ads
    sort by bid.Price descending

    winners = []
    seen_advertisers = set{}
    seen_categories = set{}

    for bid in sorted_bids:
        if len(winners) >= s.SlotsPerRotation: break
        if bid.AdvertiserID in seen_advertisers: skip
        if bid.Category in seen_categories: skip (competitive separation)

        winners.append(bid)
        seen_advertisers.add(bid.AdvertiserID)
        seen_categories.add(bid.Category)

    // Unfilled slots get publisher slate/house ads
    unfilled = s.SlotsPerRotation - len(winners)

    // Each winner gets a time slot in the rotation
    for i, winner in winners:
        winner.SlotIndex = i
        winner.SlotStart = i * s.SlotDuration
        winner.SlotEnd = (i + 1) * s.SlotDuration

    return AuctionResult{Winners: winners, Unfilled: unfilled, IsRotation: true}
}
```

#### Strategy Interface

All strategies implement the same interface:

```go
type AuctionStrategy interface {
    // Select winners from filtered, eligible bids
    Select(ctx context.Context, bids []Bid, request AuctionRequest) (AuctionResult, error)

    // Type returns the strategy type for logging/metrics
    Type() string
}

type AuctionResult struct {
    Winners       []Winner
    LossBids      []LossBid       // bids that didn't win, with reason
    ShortFill     int             // unfilled duration (pods) or slots (batch/timeslot)
    IsMultiWinner bool            // true for pod, batch, timeslot, retail
    Positions     bool            // true for retail (winners have positions)
    IsRotation    bool            // true for DOOH timeslot
}
```

The Exchange selects the strategy based on the impression type, runs the common filter pipeline, then delegates to the strategy for winner selection. This means:

- Adding a new channel = adding a new strategy implementation
- Common logic (floor prices, quality controls, competitive separation) is never duplicated
- Metrics and logging work the same across all strategies
- Loss notifications work the same across all strategies

#### Competitive Separation Across Strategies

Each strategy handles separation differently based on its context:

| Strategy | Separation scope | How |
|---|---|---|
| SingleWinner | Per page (using page_request_id) | Redis page context (30s TTL) |
| Pod | Within the pod | In-memory during pod selection |
| RelevanceWeighted | Per search results page | In-memory during selection |
| Batch | Across all billboards in one request | In-memory during selection |
| TimeSlot | Within one rotation cycle | In-memory during selection |

#### AuctionWinEvent Per Strategy

All strategies produce the same `AuctionWinEvent` for each winner, ensuring billing, reporting, and reconciliation work identically regardless of channel:

```protobuf
message AuctionWinEvent {
    int32 schema_version = 1;
    string trace_id = 2;
    string auction_id = 3;
    string strategy_type = 4;     // "single_winner", "pod", "relevance_weighted", "batch", "timeslot"
    string winner_dsp_id = 5;
    string campaign_id = 6;       // line_item_id
    string creative_id = 7;
    string placement_id = 8;
    double clearing_price = 9;
    string clearing_currency = 10;
    double clearing_price_usd = 11;
    string bid_model = 12;        // "cpm", "cpc", "cpcv", "cpi"
    int32 position = 13;          // for retail (1, 2, 3) or pod (slot index) or timeslot (slot index)
    int32 duration_seconds = 14;  // for video/audio ads
    string channel = 15;          // "display", "video", "audio", "dooh", "retail", "ingame"
}
```

The `strategy_type` and `channel` fields allow reporting and billing to handle each channel correctly while using the same event pipeline.

Implemented in `pkg/auction/`. Each strategy is a separate file:
- `pkg/auction/single.go` - SingleWinnerStrategy
- `pkg/auction/pods.go` - PodStrategy
- `pkg/auction/relevance.go` - RelevanceWeightedStrategy
- `pkg/auction/batch.go` - BatchStrategy
- `pkg/auction/timeslot.go` - TimeSlotStrategy
- `pkg/auction/strategy.go` - AuctionStrategy interface, strategy selection logic

**Configurable per deal via live config.** Default auction type is a platform-wide setting (`exchange.default_auction_type = "first-price"`) that can be overridden per deal.

#### Multi-Exchange Deployment Model

Instead of one Exchange handling all channels, deploy **specialised Exchange instances per channel**. Same code, different configuration. Each scales independently.

```
Traefik routes by path:
    /v1/openrtb/auction          -> exchange-display   (SingleWinner)
    /v1/openrtb/auction/video    -> exchange-video     (Pod + SingleWinner)
    /v1/openrtb/auction/audio    -> exchange-audio     (Pod + SingleWinner)
    /v1/openrtb/auction/dooh     -> exchange-dooh      (TimeSlot)
    /v1/openrtb/auction/retail   -> exchange-retail    (RelevanceWeighted)
    /v1/openrtb/auction/game     -> exchange-game      (Batch + SingleWinner)
```

**Why separate deployments:**

| Concern | Single Exchange | Multi-Exchange |
|---|---|---|
| Scaling | One pool handles all traffic. Hard to scale for channel-specific spikes. | Retail scales for Black Friday. DOOH stays steady. Video scales for live events. |
| Blast radius | Bug in retail auction logic takes down all channels | Retail bug only affects retail |
| Resource profile | One-size-fits-all CPU/memory | Retail gets more CPU (relevance scoring). Video pods get more memory. |
| Team ownership | One team owns everything | Channel teams can own their exchange instance |
| Latency | Shared queue, mixed workload | Dedicated queue per channel, predictable latency |

**Same binary, different config:**

All exchange instances use `cmd/exchange/` with a `--channel` flag:

```
cmd/exchange --channel=display    # loads SingleWinnerStrategy only
cmd/exchange --channel=video      # loads PodStrategy + SingleWinnerStrategy
cmd/exchange --channel=retail     # loads RelevanceWeightedStrategy, connects to product catalog
cmd/exchange --channel=dooh       # loads TimeSlotStrategy
cmd/exchange --channel=game       # loads BatchStrategy + SingleWinnerStrategy
```

Or deploy `cmd/exchange --channel=all` for local dev (one instance handles everything).

**K8s deployment:**

```
k8s/base/exchange/
    display/     # deployment + service + HPA
    video/       # deployment + service + HPA
    audio/       # deployment + service + HPA (may share with video)
    dooh/        # deployment + service + HPA
    retail/      # deployment + service + HPA
    game/        # deployment + service + HPA
```

**Local dev (Kustomize overlay):**

In `overlays/local/`, all exchange deployments can be collapsed into a single `cmd/exchange --channel=all` instance to save laptop resources. Kustomize patches replace the 6 deployments with 1.

**Shared infrastructure:**

All exchange instances:
- Publish to the same NATS subjects (`adtech.auction.win`, `adtech.auction.complete`)
- Use the same DSP fan-out (same gRPC calls to DSPs)
- Use the same `pkg/auction/` strategy implementations
- Use the same competitive separation logic (`pkg/auction/separation.go`)
- Use the same Redis for page context / rotation context
- Produce the same `AuctionWinEvent` protobuf (with `channel` field distinguishing them)

**Monitoring:**

Prometheus metrics are tagged by channel:
- `exchange_auctions_total{channel="video"}`
- `exchange_auction_latency_seconds{channel="retail"}`
- `exchange_bid_count{channel="dooh"}`

Grafana dashboard shows all channels side-by-side. Channel-specific alerts (e.g. "retail auction latency > 50ms").

### 2. Bid Shading

In first-price auctions, DSPs risk overpaying because there's no second-price protection. Bid shading is a **real-time algorithm** that reduces the submitted bid towards an estimated market-clearing price.

**The problem without bid shading:**
```
Advertiser values impression at: $5.00 CPM
Next highest bidder would bid: $2.00 CPM
Without shading: DSP bids $5.00, wins, pays $5.00 (overpaid by $3.00)
With shading:    DSP bids $2.80, wins, pays $2.80 (saved $2.20)
```

**How bid shading works:**

```
DSP calculates raw bid value: $5.00 CPM (what the impression is worth)
    |
    v
Bid shading model looks up historical data:
    - This placement's avg clearing price: $2.20
    - This time of day: +10% (peak hours)
    - This geo: -5% (lower competition)
    - Estimated clearing price: ~$2.50
    |
    v
Calculate shaded bid:
    shade_factor = 0.6 (from win-rate curve)
    shaded_bid = estimated_clearing + (raw_bid - estimated_clearing) * shade_factor
    shaded_bid = $2.50 + ($5.00 - $2.50) * 0.6 = $4.00
    |
    v
Apply bid floor: max(shaded_bid, campaign_min_bid)
    |
    v
Submit shaded bid: $4.00 (saved $1.00 vs raw bid)
```

**Win-rate curves:**

The shading model maintains win-rate curves per placement (and optionally per geo, time of day, device):

```
Bid price:  $1.00  $2.00  $3.00  $4.00  $5.00
Win rate:    5%    25%    60%    85%    95%

Optimal bid = point on curve where marginal cost of winning
              equals marginal value of the impression
```

These curves are built from auction history (win/loss data with clearing prices from loss notifications) and updated hourly by the bid optimisation pipeline.

**Shade factor adjustment:**

| Signal | Adjustment |
|---|---|
| Won easily (bid >> clearing price) | Decrease shade factor (bid lower next time) |
| Won narrowly (bid ~ clearing price) | Keep shade factor |
| Lost (bid < clearing price) | Increase shade factor (bid higher next time) |
| No data for this placement | Use category/geo average, conservative factor |

**Shading is per-campaign configurable:**

| Setting | Options |
|---|---|
| Shading mode | `aggressive` (bid closer to estimate), `moderate` (default), `conservative` (bid closer to raw value) |
| Shading enabled | Can be disabled per campaign (always bid raw value) |
| Min win rate target | "I want to win at least 40% of auctions I participate in" |
| Max bid reduction | "Never shade below 50% of raw bid value" |

**Interaction with bid modifiers:**

```
Raw bid value: $5.00
    |
    v
Apply bid modifiers: +20% mobile = $6.00
    |
    v
Apply bid shading: shade to $4.20
    |
    v
Submit: $4.20
```

Bid modifiers are applied first (they reflect the advertiser's value assessment), then shading reduces the bid to win at an efficient price.

Implemented in `pkg/auction/shading.go`. Runs in real-time in the DSP during bid evaluation. Win-rate curves stored in Redis L2 cache, updated hourly by `cmd/optimise/`.

### 3. Campaign Hierarchy

Flat campaign -> creative structure doesn't scale. An advertiser running 50 campaigns across 3 markets with different objectives needs organisation, shared budgets, and targeting inheritance.

**The hierarchy:**

```
Advertiser Account ("Acme Corp")
    |
    +-- Insertion Order ("Q3 2026 Brand Campaign")
    |       Budget: $50,000  |  Flight: Jul 1 - Sep 30
    |       |
    |       +-- Line Item ("UK Mobile Users - Brand Awareness")
    |       |       Targeting: UK + mobile  |  Bid: $2 CPM  |  Pacing: even
    |       |       |
    |       |       +-- Creative: "Banner A (300x250)"  weight: 60%
    |       |       +-- Creative: "Banner B (300x250)"  weight: 40%
    |       |
    |       +-- Line Item ("DE Desktop Users - Retargeting")
    |               Targeting: DE + desktop + existing_customers  |  Bid: $4 CPM
    |               |
    |               +-- Creative: "Banner C (728x90)"
    |
    +-- Insertion Order ("Q3 2026 Performance Campaign")
            Budget: $20,000  |  Flight: Aug 1 - Sep 30
            |
            +-- Line Item ("US Mobile - App Install CPI")
                    Targeting: US + mobile + in-market  |  Bid: $5 CPA
                    |
                    +-- Creative: "Native Ad - App Install"
```

**What each level controls:**

| Level | Owns | Inherits from parent |
|---|---|---|
| **Advertiser** | Account settings, billing, currency, industry category, competitor list | N/A |
| **Insertion Order (IO)** | Total budget pool, flight dates, overall campaign objective | Advertiser currency, industry |
| **Line Item** | Targeting, bid strategy, bid modifiers, pacing, frequency caps, shading config | IO budget, IO flight dates (can narrow but not widen) |
| **Creative** | Ad content, format, size, rotation weight, landing URL, third-party pixels | Line item targeting, bid strategy |

**Budget inheritance and sharing:**

```
IO Budget: $50,000
    |
    +-- Line Item A: no sub-budget set -> draws from IO pool
    +-- Line Item B: sub-budget $10,000 -> capped, remainder available to others
    +-- Line Item C: no sub-budget set -> draws from IO pool
```

Rules:
- IO has a total budget. Line items draw from it.
- Line items can optionally have a sub-budget (cap). Without one, they share the IO pool.
- If Line Item A underspends, the surplus is available to Line Item C (shared pool).
- Line Item B can never exceed $10,000 even if the IO pool has surplus.
- IO daily cap distributes proportionally across line items based on their pacing targets.
- Budget checks happen at both IO level and line item level (if sub-budget set).

**Targeting inheritance:**

```
IO targeting: geo = [UK, DE, FR]
    |
    +-- Line Item A: geo = [UK], device = [mobile]
    |   (narrowed: UK only, mobile only - valid, subset of IO)
    |
    +-- Line Item B: geo = [US]
        (INVALID: US is not in IO's geo list - rejected at creation)
```

Rules:
- Targeting inherits down and can only be **narrowed**, never widened.
- A line item cannot target geos/devices/segments outside what the IO allows.
- If the IO has no targeting (empty = all), line items can target anything.
- Validation happens at line item creation/update - the API rejects invalid narrowing.

**Lifecycle inheritance:**

| Event | Cascading effect |
|---|---|
| IO paused | All line items under it paused |
| IO ended | All line items under it ended |
| IO budget depleted | All line items stop bidding |
| Line item paused | Only that line item paused, others continue |
| Advertiser account suspended | All IOs and line items stopped |

**Reporting rolls up the hierarchy:**

| Query level | What you see |
|---|---|
| Creative | Performance of one specific creative |
| Line Item | Aggregated performance of all creatives in that line item |
| Insertion Order | Aggregated performance of all line items (total spend vs IO budget) |
| Advertiser | All IOs, total spend, overall ROAS |

**Database model:**

```sql
-- Insertion Order
CREATE TABLE insertion_orders (
    id UUID PRIMARY KEY,
    account_id UUID NOT NULL,        -- tenant isolation
    name TEXT NOT NULL,
    budget DECIMAL NOT NULL,
    daily_budget DECIMAL,
    start_date DATE NOT NULL,
    end_date DATE NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft',
    objective TEXT,                   -- brand_awareness, performance, retargeting
    currency TEXT NOT NULL DEFAULT 'USD'
);

-- Line Item (replaces flat "campaign" concept)
CREATE TABLE line_items (
    id UUID PRIMARY KEY,
    account_id UUID NOT NULL,
    insertion_order_id UUID NOT NULL REFERENCES insertion_orders(id),
    name TEXT NOT NULL,
    sub_budget DECIMAL,              -- optional cap, NULL = shared IO pool
    bid_strategy TEXT NOT NULL,      -- cpm, cpc, cpa, vcpm
    base_bid DECIMAL NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft',
    pacing TEXT NOT NULL DEFAULT 'even',
    shading_mode TEXT DEFAULT 'moderate'
);

-- Creative assignment (many-to-many with rotation weight)
CREATE TABLE line_item_creatives (
    line_item_id UUID NOT NULL REFERENCES line_items(id),
    creative_id UUID NOT NULL REFERENCES creatives(id),
    weight INT NOT NULL DEFAULT 100,
    PRIMARY KEY (line_item_id, creative_id)
);
```

**API impact:**

The existing campaign endpoints become line item endpoints. New IO endpoints are added:

- `GET/POST       /v1/api/insertion-orders` - IO CRUD
- `GET/PUT/DELETE /v1/api/insertion-orders/{id}` - IO management
- `GET            /v1/api/insertion-orders/{id}/line-items` - line items under an IO
- Existing `/v1/api/campaigns/*` endpoints map to line items (campaigns = line items in our model)

**Note:** Throughout the rest of this plan, "campaign" generally refers to the **line item** level - the entity that has targeting, bids, and creatives. The IO is the budget/scheduling container above it.

### 4. Frequency Capping Granularity

Frequency capping controls how many times a user sees an ad. Without it, one user gets bombarded while others see nothing. Caps apply at multiple dimensions simultaneously.

**Cap dimensions:**

| Dimension | Example rule | Redis key | TTL |
|---|---|---|---|
| Per user per line item | "Max 3/day for this line item" | `fc:{user}:{line_item}:d` | 24h |
| Per user per creative | "Max 2/day for this creative" | `fc:{user}:{creative}:d` | 24h |
| Per user per IO | "Max 8/day across all line items in this IO" | `fc:{user}:{io}:d` | 24h |
| Per user per advertiser | "Max 10/day across all of Acme's ads" | `fc:{user}:{advertiser}:d` | 24h |
| Per user per placement | "Max 1/hour on this placement" | `fc:{user}:{placement}:h` | 1h |
| Lifetime per line item | "Max 20 total for this line item ever" | `fc:{user}:{line_item}:life` | No TTL |
| Per user per line item per hour | "Max 1/hour for this line item" | `fc:{user}:{line_item}:h` | 1h |
| Per user per line item per week | "Max 7/week for this line item" | `fc:{user}:{line_item}:w` | 7d |

**Time windows:**

| Window | TTL | Resets |
|---|---|---|
| Per hour | 1h | Rolling 1h window |
| Per day | 24h | Rolling 24h (or midnight UTC reset, configurable) |
| Per week | 7d | Rolling 7d |
| Lifetime | No TTL | Never resets |

Multiple time windows can apply to the same line item simultaneously: "max 1/hour AND max 5/day AND max 15/week AND max 50 lifetime".

**How capping works at serve time:**

```
Ad Server receives winning bid for user X, line item Y, creative Z
    |
    v
Check ALL applicable frequency caps in Redis (pipelined, single round trip):
    fc:{userX}:{lineItemY}:h  -> current: 0, cap: 1  -> PASS
    fc:{userX}:{lineItemY}:d  -> current: 2, cap: 5  -> PASS
    fc:{userX}:{creativeZ}:d  -> current: 1, cap: 2  -> PASS
    fc:{userX}:{advertiser}:d -> current: 7, cap: 10 -> PASS
    fc:{userX}:{lineItemY}:life -> current: 18, cap: 20 -> PASS
    |
    v
ALL pass? -> Serve the ad, increment all counters (INCR, pipelined)
ANY fail? -> Don't serve this ad. Try next eligible line item/creative.
             If no eligible ads remain, serve default/fallback ad.
```

**Redis efficiency:**

All cap checks for a single serve decision are pipelined into one Redis round trip (5-8 INCR commands batched). This adds ~1ms to serve time, not one round trip per check.

```go
// Pipelined frequency cap check (pseudocode)
pipe := redis.Pipeline()
results := []pipe.Get(capKey) for capKey in allApplicableCaps
pipe.Exec()
// Check all results, if all pass:
pipe2 := redis.Pipeline()
for capKey in allApplicableCaps { pipe2.Incr(capKey) }
pipe2.Exec()
```

**What happens when a cap is hit:**

```
User X has seen line item Y 5 times today (daily cap = 5)
    |
    v
Ad Server receives winning bid for user X, line item Y
    |
    v
Frequency cap check: fc:{userX}:{lineItemY}:d = 5/5 -> BLOCKED
    |
    v
Fallback logic:
    1. Is there another creative in this line item? Try it (creative-level cap may not be hit)
    2. Is there another line item from same IO that matches? Try it
    3. Is there another advertiser's line item? Try it
    4. No eligible ads? Serve publisher's default/fallback ad
```

**Cap configuration in the API:**

```json
{
    "line_item_id": "li_123",
    "frequency_caps": [
        {"dimension": "line_item", "window": "hour", "limit": 1},
        {"dimension": "line_item", "window": "day", "limit": 5},
        {"dimension": "line_item", "window": "week", "limit": 15},
        {"dimension": "line_item", "window": "lifetime", "limit": 50},
        {"dimension": "creative", "window": "day", "limit": 3},
        {"dimension": "advertiser", "window": "day", "limit": 10}
    ]
}
```

**Frequency capping and privacy:**

- Caps require a user ID (platform ID from cookie). If the user has no cookie or has opted out, frequency capping cannot be enforced.
- Without a user ID: ad is served with no cap check, but a Prometheus metric tracks "uncapped impressions" so the advertiser knows what percentage of their spend is uncapped.
- Cross-device frequency capping works via the identity graph - if the same hashed email is seen on mobile and desktop, caps apply across both devices.

**Reporting on frequency:**

- Frequency distribution report: "X% of users saw this ad 1 time, Y% saw it 2 times, Z% saw it 3+ times"
- Over-frequency alert: if a significant % of users are hitting caps, the line item may need a broader audience or lower budget
- Under-delivery alert: if caps are blocking too many impressions, the line item won't spend its budget

Implemented in `cmd/adserver/freqcap.go` (Redis key `adserver:freqcap:{user|hh:…}:{campaign}`). Enforced per-user AND per-household, plus the advertiser's own limit/window from the warm `freq_caps` cache. Applies across every format: display is capped in the ad server's render call; video/native/audio (rendered by the publisher-adserver) are capped via the SSP's cap-only ad-server call before the winner is returned. The richer video dimensions below (per-session / per-content / per-pod) are PLANNED, not built.

### 5. Targeting Exclusions

Every targeting dimension supports both inclusion (show to) and exclusion (don't show to). Exclusions are critical for avoiding waste - "show to everyone in Europe except Germany" or "target car enthusiasts but exclude existing customers."

**Available targeting dimensions (include and exclude):**

| Dimension | Include example | Exclude example |
|---|---|---|
| **Geo** (country/region/city/postcode) | UK, DE, FR | UK_scotland, DE_berlin |
| **Device type** | mobile, tablet | desktop |
| **OS** | iOS, Android | - |
| **Browser** | Chrome, Safari | - |
| **Connection type** | wifi | cellular (save budget, video won't load well) |
| **Audience segments** | sports_fans, in_market_cars | existing_customers (acquisition only) |
| **Domains / URLs** | premium_publishers_list | competitor.com, low_quality_sites |
| **App bundles** | com.nytimes.app | com.spam.app |
| **IAB content categories** | IAB17 (Sports), IAB1 (Arts) | IAB25 (Non-standard), IAB26 (Illegal) |
| **Time of day (dayparting)** | 09:00-17:00 | 00:00-06:00 |
| **Day of week** | Mon-Fri | Sat, Sun |
| **Language** | en, de | - |
| **Publisher IDs** | Specific publishers | Specific publishers |
| **Placement IDs** | Specific placements | Specific placements |
| **Inventory type** | site (web) | app (or vice versa) |
| **Deal IDs** | Specific deals only | - |
| **Custom key-values** | page_section=sports | page_section=opinion |

**Evaluation logic:**

```
Bid request arrives with: geo=UK_london, device=mobile, segments=[sports_fans, existing_customers]

Line item targeting:
  include:
    geo: [UK]                     -> MATCH (UK_london is in UK)
    device: [mobile, tablet]      -> MATCH (mobile)
    segments: [sports_fans]       -> MATCH (user has sports_fans)
  exclude:
    geo: [UK_scotland]            -> NO MATCH (london != scotland) -> OK
    segments: [existing_customers] -> MATCH (user is existing_customer) -> EXCLUDED

Result: EXCLUDED (user matches an exclusion rule)
```

**Evaluation order:**

```
Bid request arrives
    |
    v
1. Check inclusions: does the request match at least one value
   in every included dimension?
    +-- No  -> No bid (doesn't match targeting)
    |
    +-- Yes -> Continue
    |
    v
2. Check exclusions: does the request match any excluded value
   in any dimension?
    +-- Yes -> No bid (explicitly excluded)
    |
    +-- No  -> Continue
    |
    v
3. Targeting matched -> evaluate bid
```

**Inclusion logic is AND across dimensions, OR within a dimension:**
- `geo: [UK, DE]` AND `device: [mobile]` means: (UK OR DE) AND (mobile)
- User must match at least one value in every dimension that has inclusions

**Exclusion logic is OR across everything:**
- If the user matches ANY exclusion in ANY dimension, they're excluded

**Audience suppression lists:**

A special case of exclusion. Advertisers upload "do not target" lists:

| List type | Use case |
|---|---|
| Existing customers | Acquisition campaigns - don't waste budget on people who already converted |
| Recent converters | Don't show ads to people who just bought (annoying + wasted spend) |
| Opt-out list | Users who said "stop showing me this ad" |
| Employee list | Don't target your own employees |

These are uploaded via the audience API (`POST /v1/api/audiences/upload`) with a `suppression: true` flag. The DSP treats them as exclusion segments during targeting evaluation.

**Targeting inheritance with exclusions:**

```
IO targeting:
  include: geo=[UK, DE, FR]
  exclude: segments=[do_not_target_list]

Line Item targeting:
  include: geo=[UK], device=[mobile]         # narrowed from IO
  exclude: segments=[existing_customers]     # additional exclusion

Effective targeting for this line item:
  include: geo=[UK], device=[mobile]
  exclude: segments=[do_not_target_list, existing_customers]  # merged
```

Exclusions from parent levels are always inherited and cannot be removed. Line items can add more exclusions but cannot un-exclude something the IO excluded.

**Targeting estimation:**

Before activating a line item, the advertiser can preview the estimated reach:

```
POST /v1/api/targeting/estimate
Body: { include: {geo: [UK], device: [mobile]}, exclude: {segments: [existing_customers]} }
Response: { estimated_daily_impressions: 150000, estimated_unique_users: 45000 }
```

Based on historical impression volume for matching placements. Helps advertisers set realistic budgets.

Implemented in `pkg/targeting/`. Evaluation is hot-path code (runs on every bid request) so it must be fast - targeting rules are loaded into L1 cache from Postgres via NATS invalidation.

### 6. Multi-Currency

Advertisers in New York pay in USD. Publishers in London set floor prices in GBP. An advertiser in Tokyo bills in JPY. The platform handles all conversions transparently - no one thinks about exchange rates.

**Where currency is set:**

| Entity | Currency set by | Set when | Can change? |
|---|---|---|---|
| Advertiser account | Advertiser at registration | Account creation | No (locked once billing starts) |
| Publisher account | Publisher at registration | Account creation | No (locked once payouts start) |
| Insertion Order | Inherits from advertiser | IO creation | No |
| Line Item | Inherits from IO | Auto | No |
| Placement floor price | Publisher's currency | Placement creation | Yes (same currency) |
| Bid request | Publisher's currency | Auto | Auto |
| DSP bid | Advertiser's currency internally, converted to bid request currency for submission | Auto | Auto |
| Auction clearing price | Bid request currency (publisher's) | Auto | Auto |
| Invoice | Advertiser's currency | Auto | No |
| Payout | Publisher's currency | Auto | No |
| Reporting | Selectable (original currency or USD normalised) | Query time | Yes |

**Conversion flow during an auction:**

```
Advertiser (USD) bids on Publisher (GBP) inventory:

1. Line item base bid: $3.00 USD CPM
2. Bid modifiers applied: $3.60 USD (mobile +20%)
3. Bid shading applied: $2.80 USD
4. Convert to bid request currency: $2.80 USD * 0.79 = £2.21 GBP
5. Submit bid to Exchange: £2.21 GBP
6. Exchange runs auction in GBP
7. Winner at: £2.21 GBP (first-price)
    |
    v
8. AuctionWinEvent records BOTH:
    clearing_price: 2.21
    clearing_currency: GBP
    clearing_price_usd: 2.80  (USD equivalent at today's rate)
    |
    v
9. DSP decrements budget: $2.80 USD (advertiser's currency)
10. Publisher earns: £2.21 * (1 - platform_fee) GBP
11. Invoice shows: $2.80 USD
12. Payout shows: £1.77 GBP (at 20% platform fee)
```

**Exchange rate management:**

| Concern | Approach |
|---|---|
| Rate source | Configurable via live config. Default: ECB (European Central Bank) daily rates. Can switch to live feed. |
| Update frequency | Daily CronJob fetches rates at 00:00 UTC. Stored in Postgres `exchange_rates` table. |
| Rate caching | Loaded into L1 cache on all services. Invalidated via NATS when new rates arrive. |
| Rate locking for billing | Daily spend snapshots lock the rate used that day. Invoices use the rate from the day the spend occurred, not the invoice date. |
| Supported currencies | All ISO 4217 currencies. Common: USD, EUR, GBP, JPY, AUD, CAD, CHF, CNY, INR, BRL. |
| Base currency | USD internally for consistent cross-currency reporting and rollups. |

**Every event record stores dual amounts:**

```sql
-- Example: impressions table in analytics store
clearing_price       DECIMAL  -- in publisher's currency (original)
clearing_currency    TEXT     -- ISO 4217 code (GBP)
clearing_price_usd   DECIMAL  -- USD equivalent (normalised)
```

This means reporting can show:
- "Campaign spent $5,000 USD today" (advertiser's currency)
- "Placement earned £3,200 GBP today" (publisher's currency)
- "Platform revenue: $1,800 USD" (normalised to base currency)

Without re-converting at query time (rates may have changed).

**Edge cases:**

| Scenario | Handling |
|---|---|
| Rate not available for a currency | Reject bid (don't guess). Alert ops team. |
| Rate changes mid-day | All auctions use the rate locked at 00:00 UTC. Next day uses new rate. No mid-day surprises. |
| Advertiser budget nearly depleted | Convert remaining budget to all active bid request currencies. Stop bidding when any conversion shows insufficient funds. |
| Currency mismatch in deal | Deal price is in publisher's currency. DSP converts to compare against their budget. |
| Reporting across currencies | Default view: advertiser sees their currency, publisher sees theirs. Platform admin sees USD normalised. |

**Database:**

```sql
CREATE TABLE exchange_rates (
    base_currency TEXT NOT NULL DEFAULT 'USD',
    target_currency TEXT NOT NULL,
    rate DECIMAL NOT NULL,
    effective_date DATE NOT NULL,
    source TEXT NOT NULL,  -- 'ecb', 'live_feed', 'manual'
    PRIMARY KEY (base_currency, target_currency, effective_date)
);
```

**Implementation:**

| Component | Location |
|---|---|
| Conversion functions | `pkg/currency/convert.go` - `Convert(amount, from, to, date) -> amount` |
| Rate storage | `pkg/currency/rates.go` - Postgres read/write, L1 cache |
| Rate update job | `cmd/rollup/` (or dedicated CronJob) - daily fetch from rate source |
| Rate config | Live config: `currency.rate_source`, `currency.supported_currencies` |

### 7. Native Ad Format

Native ads look like editorial content on the publisher's site rather than a rectangular banner. The advertiser provides structured data fields (title, image, description) and the publisher renders them in their own style/layout. Higher engagement than display because they don't look like ads.

**How native differs from display:**

| Concern | Display (banner) | Native |
|---|---|---|
| Creative | Pre-rendered image/HTML at fixed size (300x250, 728x90) | Structured data fields rendered by publisher |
| Appearance | Looks like an ad (banner shape, distinct from content) | Blends into page content, matches publisher's design |
| Rendering | Ad Server returns HTML/image, publisher embeds as-is | Ad Server returns JSON, publisher's template renders it |
| Sizes | Fixed IAB standard sizes | Flexible - adapts to publisher's layout |
| CTR | Typically 0.1-0.3% | Typically 0.5-1.5% (higher engagement) |

**Native ad assets (what the advertiser provides):**

| Asset | Required | Constraints | Example |
|---|---|---|---|
| `title` | Yes | Max 90 chars | "Best Running Shoes 2026" |
| `description` | Yes | Max 300 chars | "Lightweight, comfortable, built for speed." |
| `sponsored` | Yes | Advertiser name, max 50 chars | "Acme Sports" |
| `icon` | Yes | Square, min 100x100px | Brand logo |
| `main_image` | Yes | Min 600x315px (1.91:1 ratio) | Hero product image |
| `cta` | Yes | Max 20 chars | "Shop Now", "Learn More", "Download" |
| `click_url` | Yes | Valid URL, not blocklisted | Landing page |
| `rating` | No | 0-5, one decimal | "4.5" (app install ads) |
| `price` | No | Formatted string | "$49.99", "Free" |
| `video_url` | No | MP4 URL for video native | Product video |

**OpenRTB Native bid request (what the publisher asks for):**

The SSP includes a native object in the bid request describing which assets the publisher's layout needs:

```json
{
    "imp": [{
        "id": "1",
        "native": {
            "request": {
                "ver": "1.2",
                "assets": [
                    {"id": 1, "required": 1, "title": {"len": 90}},
                    {"id": 2, "required": 1, "img": {"type": 3, "w": 600, "h": 315}},
                    {"id": 3, "required": 1, "data": {"type": 2, "len": 300}},
                    {"id": 4, "required": 1, "data": {"type": 1}},
                    {"id": 5, "required": 0, "img": {"type": 1, "w": 100, "h": 100}}
                ]
            }
        }
    }]
}
```

**OpenRTB Native bid response (what the DSP returns):**

```json
{
    "native": {
        "ver": "1.2",
        "assets": [
            {"id": 1, "title": {"text": "Best Running Shoes 2026"}},
            {"id": 2, "img": {"url": "https://cdn.example.com/hero.jpg", "w": 600, "h": 315}},
            {"id": 3, "data": {"value": "Lightweight, comfortable, built for speed."}},
            {"id": 4, "data": {"value": "Acme Sports"}},
            {"id": 5, "img": {"url": "https://cdn.example.com/icon.png", "w": 100, "h": 100}}
        ],
        "link": {"url": "https://acme.com/shoes", "clicktrackers": ["https://.../v1/t/click?..."]},
        "imptrackers": ["https://.../v1/t/imp?..."],
        "jstracker": "<script>/* viewability */</script>"
    }
}
```

**Publisher-side rendering:**

The `adtech.js` SDK receives the native response and renders it using the publisher's template:

```html
<!-- Publisher's native ad template -->
<div class="native-ad" style="/* matches publisher's content style */">
    <img src="{{icon}}" class="ad-icon">
    <div class="ad-content">
        <span class="ad-sponsored">Sponsored by {{sponsored}}</span>
        <h3>{{title}}</h3>
        <p>{{description}}</p>
        <a href="{{click_url}}" class="ad-cta">{{cta}}</a>
    </div>
    <img src="{{main_image}}" class="ad-hero">
</div>
```

The publisher controls the styling. The platform controls the content and tracking.

**Creative review for native:**

| Check | Rule |
|---|---|
| Title length | <= 90 chars |
| Description length | <= 300 chars |
| CTA text | Must be from approved list or <= 20 chars |
| Main image | Min 600x315, correct aspect ratio, not blurry |
| Icon | Square, min 100x100 |
| Landing URL | Reachable, not blocklisted, matches advertiser's domain |
| "Sponsored" label | Must be present (regulatory requirement in many jurisdictions) |
| Content | Auto-scan for policy violations |

**Native ad reporting:**

Same metrics as display (impressions, clicks, CTR, conversions) but also:
- Asset-level performance: which title/image/CTA combinations perform best
- Feeds into creative A/B testing - rotate different titles or images within the same native ad

Implemented in `pkg/openrtb/native.go` for request/response types. `web/static/adtech.js` handles client-side rendering. Creative validation in `pkg/adserving/`.

### 8. OpenRTB `app` Object (Mobile App Inventory)

Mobile app inventory is a large share of programmatic advertising. The bid request uses an `app` object instead of `site` when the ad is shown inside a mobile app. The platform must handle both seamlessly.

**`site` (web) vs `app` (mobile) bid request:**

```json
// Web inventory
{
    "site": {
        "domain": "nytimes.com",
        "name": "The New York Times",
        "page": "https://nytimes.com/2026/05/27/sports/football.html",
        "cat": ["IAB17"],
        "publisher": {"id": "pub_123", "name": "NYT Digital"}
    }
}

// Mobile app inventory
{
    "app": {
        "bundle": "com.nytimes.ios",
        "name": "NYTimes - Breaking News",
        "storeurl": "https://apps.apple.com/app/id284862083",
        "cat": ["IAB17"],
        "ver": "10.5.2",
        "publisher": {"id": "pub_123", "name": "NYT Digital"}
    }
}
```

**Key differences that affect the platform:**

| Concern | Web (`site`) | App (`app`) |
|---|---|---|
| Identifier | `domain` | `bundle` (e.g. com.nytimes.ios) |
| Page context | `page` URL available for contextual targeting | No page URL - use app category and content signals |
| User identity | First-party cookie (platform ID) | Device advertising ID (IDFA/GAID) or no ID (ATT opted out) |
| Ad format | Display (banner), native | Display, native, interstitial, rewarded (future) |
| Authorised seller verification | `ads.txt` at domain root | `app-ads.txt` at developer's domain |
| Ad tag | JavaScript tag (`adtech.js`) | SDK integration (future - simulated for now) |
| Viewability | IntersectionObserver in browser | SDK-reported (OMSDK - future) |
| Click handling | Opens URL in browser tab | Opens URL in in-app browser or deep link |

**Impact on each service:**

| Service | What changes for app inventory |
|---|---|
| **SSP** | Stores `bundle` instead of `domain`. Generates bid request with `app` object. |
| **Exchange** | Checks `app-ads.txt` (not `ads.txt`). Targeting engine matches against app categories and bundle IDs. |
| **DSP** | Targeting supports `app_bundle` and `app_category` dimensions. Can target specific apps or app categories. |
| **Tracker** | Same pixel endpoints work. Device ID passed as additional parameter if available. |
| **Ad Server** | Click handling may differ (deep links for app install campaigns). |
| **Fraud** | Different fraud signals for apps (SDK spoofing, device farm detection). |
| **Quality Controls** | Publishers can blocklist app bundles (`bapp`). Advertisers can target/exclude specific bundles. |

**Targeting for app inventory:**

```yaml
targeting:
  include:
    inventory_type: [app]                    # app only (or [site] for web only, or both)
    app_categories: [IAB17, IAB1]            # sports and arts apps
    app_bundles: [com.nytimes.ios]           # specific app
    os: [iOS]
    device: [mobile]
  exclude:
    app_bundles: [com.spam.app]
```

**app-ads.txt validation:**

Same as `ads.txt` but for apps. The `app-ads.txt` file lives at the app developer's website domain (declared in the app store listing):

```
App store listing: developer website = "nytimes.com"
Exchange checks: https://nytimes.com/app-ads.txt
    -> Is our platform listed as authorised seller? -> Proceed or reject
```

The ads.txt crawler (`cmd/adstxt/`) handles both `ads.txt` and `app-ads.txt`. For app publishers, the crawler resolves the developer website URL from the app store listing (or from publisher registration).

**Device advertising IDs:**

| ID | Platform | Status |
|---|---|---|
| IDFA | iOS | Requires user opt-in via ATT (App Tracking Transparency). ~30% opt-in rate. |
| GAID | Android | Available but Google planning deprecation. |
| No ID | Both | User opted out or ID not available. Contextual targeting only. |

When a device ID is available, it can be used alongside or instead of the platform cookie for frequency capping and identity graph linking. When not available, the ad is served with contextual targeting only (same as cookie-less web).

```json
{
    "device": {
        "ifa": "AEBE52E7-03EE-455A-B3C4-E57283966239",  // IDFA or GAID
        "lmt": 0,    // 0 = tracking allowed, 1 = limited
        "os": "iOS",
        "osv": "17.5",
        "ua": "...",
        "ip": "..."
    }
}
```

**For MVP:** We simulate app inventory via bid requests with `app` objects in the simulator. No native mobile SDK yet (deferred). The SSP, Exchange, and DSP all handle `app` objects correctly. The simulator generates both `site` and `app` bid requests.

Implemented in `pkg/openrtb/` - `app.go` alongside `site.go`. Targeting in `pkg/targeting/` handles both inventory types. ads.txt crawler handles both `ads.txt` and `app-ads.txt`.

### 9. OpenRTB `regs` Object (Regulatory Signals)

Privacy regulation varies by geography and is getting stricter. The bid request carries regulatory signals so every service in the chain knows what's allowed for this specific request. Getting this wrong means legal liability.

**Full `regs` + `user` consent object in bid request:**

```json
{
    "regs": {
        "coppa": 0,
        "ext": {
            "gdpr": 1,
            "us_privacy": "1YNN"
        }
    },
    "user": {
        "id": "uuid-123",
        "ext": {
            "consent": "CO2vY5CO2vY5CAOACAENALCAAAAAK7AAA..."
        }
    }
}
```

**Regulatory signals and their effects:**

| Signal | Source | When set | Platform effect |
|---|---|---|---|
| `coppa=1` | Publisher declares content is child-directed | Publisher registers site/app as child-directed | **Strictest**: No user ID, no tracking, no behavioural targeting, no frequency capping, no identity graph. Contextual targeting only. No personal data collected. |
| `gdpr=1` | User is in EU/EEA (detected by geo) | SSP sets based on user's IP geolocation | Check TCF consent string before any user-level processing |
| `us_privacy` | CCPA/US state laws | SSP sets based on user's IP (California, Virginia, etc.) | Respect opt-out of sale/sharing. Format: `1YNN` = version 1, notice given, no opt-out, no LSPA |
| `consent` (TCF string) | User's consent choices from the publisher's CMP | Publisher's Consent Management Platform | Encodes which vendors and purposes have consent (deferred for full parsing, but field carried through) |

**How the platform processes each signal:**

```
Bid request arrives
    |
    v
pkg/privacy reads regs + user.consent
    |
    v
Build PrivacyContext:
    coppa:     true/false
    gdpr:      true/false
    consent:   parsed TCF string (or nil if not present)
    us_optout: true/false (from us_privacy string)
    |
    v
PrivacyContext flows through entire chain via gRPC metadata
    |
    v
Every service checks PrivacyContext before user-level operations
```

**What each restriction level means in practice:**

| Operation | No restrictions | GDPR (consent given) | GDPR (no consent) | COPPA | US opt-out |
|---|---|---|---|---|---|
| Set/read platform cookie | Yes | Yes | No | No | Yes |
| User-level targeting (segments, retargeting) | Yes | Yes | No | No | No |
| Frequency capping | Yes | Yes | No | No | No |
| Identity graph (cross-site/device) | Yes | Yes | No | No | No |
| Contextual targeting (geo, page content) | Yes | Yes | Yes | Yes | Yes |
| Store user-level event data | Yes | Yes | Aggregated only | No | Aggregated only |
| Pass user ID in bid request | Yes | Yes | No (anonymised) | No | Yes (but no sale) |
| Conversion attribution (user-level) | Yes | Yes | No | No | No |
| Viewability tracking | Yes | Yes | Yes | Yes | Yes |

**SSP responsibility - setting the signals:**

The SSP determines which regulations apply based on:

| Signal | How SSP determines it |
|---|---|
| `coppa` | Publisher declares at site/app registration: "this is child-directed content" |
| `gdpr` | User's IP geolocates to EU/EEA country. SSP uses geo lookup from `pkg/identity/`. |
| `us_privacy` | User's IP geolocates to a US state with privacy law. SSP reads opt-out cookie if present. |
| `consent` | Publisher's CMP (Consent Management Platform) sets a TCF consent cookie. SSP reads it. |

**DSP responsibility - respecting the signals:**

The DSP must not bid with user-level targeting if consent is absent:

```
PrivacyContext says: gdpr=true, consent=nil (no consent given)
    |
    v
DSP bid evaluation:
    - Skip user segment matching (can't use audience data)
    - Skip retargeting (can't use identity graph)
    - Skip frequency cap check (can't identify user)
    - CAN still bid based on: page content, geo (country-level), device type, time of day
    - If no contextual match -> no bid
    - If contextual match -> bid with contextual-only strategy (usually lower bid)
```

**Tracker responsibility - limiting data storage:**

```
PrivacyContext says: coppa=true
    |
    v
Tracker receives impression pixel:
    - Do NOT store user ID in event record (set to "anon")
    - Do NOT write to identity graph
    - Do NOT increment frequency cap counters
    - DO record impression (for billing and publisher reporting)
    - DO record geo at country level only (not city/postcode)
    - DO record device type (not full UA fingerprint)
```

**Consent status in reporting:**

Events are tagged with their consent status so reporting can break down:
- "X% of impressions had full consent (user-level targeting)"
- "Y% had partial consent (contextual only)"
- "Z% were COPPA (child-directed, no user data)"

This helps advertisers understand what portion of their reach is targetable vs contextual-only.

**Audit trail:**

Every privacy decision is logged in the audit system:
- "Suppressed user targeting for trace_id X: gdpr=true, consent=absent"
- "Anonymised user ID for trace_id Y: coppa=true"

This provides compliance evidence if regulators ask "how do you handle requests without consent?"

**Implementation:**

| Component | Location |
|---|---|
| Privacy context builder | `pkg/privacy/context.go` - reads regs + consent, builds PrivacyContext |
| Privacy middleware | `pkg/middleware/privacy.go` - injects PrivacyContext into gRPC metadata |
| Privacy checks | `pkg/privacy/checks.go` - `CanTrackUser()`, `CanTargetUser()`, `CanStoreUserData()` |
| TCF parsing (deferred) | `pkg/privacy/tcf.go` - parse consent string for vendor/purpose consent |
| Geo-based regulation detection | `pkg/privacy/geo.go` - IP -> country -> which regulations apply |
| Consent signal in OpenRTB | `pkg/openrtb/regs.go` - regs + user.consent fields |

### 9a. User Opt-Out and Data Deletion System

Opt-outs are not just a flag - they require propagation to every system holding user data, verification that nothing was missed, and an audit trail proving compliance.

#### Opt-Out Levels

| Level | What it means | Triggered by | Reversible? |
|---|---|---|---|
| **Level 1: No personalisation** | Stop targeting this user with behavioural/audience data. Contextual ads still shown. Keep anonymous event data. Don't delete history. | CMP opt-out, "don't personalise" setting | Yes |
| **Level 2: No tracking** | Stop all tracking. Delete platform cookie. No frequency cap, no identity graph, no user-level events. Ads served but fully anonymous. | Device `lmt=1`, platform opt-out page | Yes |
| **Level 3: Full deletion** | Delete everything. Anonymise all historical data. Purge from all systems. As if the user never existed. | GDPR Article 17 request, CCPA deletion request | No - irreversible |

Each level is a superset of the previous.

#### Where User Data Lives (Deletion Map)

Every system that holds user data must be included in the opt-out propagation:

| System | Data held | Level 1 action | Level 2 action | Level 3 action |
|---|---|---|---|---|
| **Browser cookie** | Platform ID (`adtech_uid`) | Keep cookie, flag as "no personalisation" | Delete cookie | Delete cookie |
| **Identity graph** (Postgres) | User edges (cross-publisher, cross-device links) | Remove from audience segments, keep graph | Delete all edges | Delete all edges |
| **Audience segments** (Postgres) | User membership in advertiser/publisher segments | Remove from behavioural segments, keep contextual | Remove from all segments | Remove from all segments |
| **Advertiser CRM matches** (Postgres) | Hashed email match to advertiser customer lists | Remove match records | Remove match records | Remove match records |
| **Redis frequency caps** | `fc:{user}:*` keys | Keep (capping still applies for now) | Delete all `fc:{user}:*` keys | Delete all `fc:{user}:*` keys |
| **Redis session/cache** | Any user-keyed cache entries | Keep | Delete | Delete |
| **Analytics store** (DuckDB/ClickHouse) | Event records with `user_id` | Keep (data still attributed but not used for targeting) | Anonymise: set `user_id = "anon"` in future events | Anonymise: set `user_id = "anon"` in ALL historical events |
| **Parquet/Delta files** | Enriched data zone with user attributes | Keep | Anonymise in future files | Rewrite historical files with user anonymised |
| **L1 caches** (in-process) | Cached user segments, identity lookups | Invalidate via NATS | Invalidate via NATS | Invalidate via NATS |
| **NATS in-flight messages** | Events currently in streams | Cannot recall | Future events anonymised | Cannot recall historical, future anonymised |
| **Audit log** | Records of user data processing | Keep (legal requirement) | Keep (legal requirement) | Keep (legal requirement - proof of deletion) |

#### Opt-Out Flow

**Level 1 & 2 (real-time, immediate effect):**

```
User opts out (via CMP, platform page, or device setting)
    |
    v
Gateway receives opt-out request
    POST /v1/api/privacy/opt-out
    Body: {user_id: "uuid-123", level: 2}
    |
    v
Gateway publishes to NATS: adtech.privacy.opt_out
    {user_id: "uuid-123", level: 2, timestamp: "..."}
    |
    v
All services consume and act immediately:
    +-- DSP: remove from audience segments, clear targeting cache
    +-- Ad Server: delete frequency cap keys in Redis
    +-- Tracker: flag user in Redis blocklist (future events anonymised)
    +-- Reporting: future events for this user stored with user_id="anon"
    +-- Identity service: delete graph edges (Level 2)
    |
    v
Opt-out status stored in Postgres: opt_out_registry table
    {user_id, level, timestamp, source}
    |
    v
Platform cookie updated (Level 1) or deleted (Level 2)
    via Set-Cookie in response
```

**Level 3 (async, requires data rewriting):**

```
User submits deletion request
    POST /v1/api/privacy/delete
    Body: {user_id: "uuid-123", verification: "email_confirmed"}
    |
    v
All Level 2 actions execute immediately (real-time)
    |
    v
Deletion job queued (K8s Job):
    1. Scan analytics store for all events with user_id = "uuid-123"
    2. UPDATE SET user_id = "anon", anonymise IP, clear device fingerprint
    3. Scan Parquet/Delta files in enriched zone
    4. Rewrite files with user anonymised (Delta Log tracks the change)
    5. Scan and remove from all audience segment tables
    6. Remove all identity graph edges
    7. Remove all advertiser CRM match records
    8. Scan Redis for any remaining user-keyed data
    |
    v
Deletion verification job:
    1. Query every data store for user_id = "uuid-123"
    2. Assert zero results across all systems
    3. If any found -> alert, re-run deletion for that system
    4. If clean -> mark deletion as verified in audit log
    |
    v
Audit record:
    {action: "user_deletion_completed", user_id: "uuid-123",
     systems_purged: ["identity_graph", "analytics", "parquet", "audiences", "redis", "crm_matches"],
     verification: "passed", timestamp: "..."}
```

#### Opt-Out Registry

Central registry tracks every user's opt-out status:

```sql
CREATE TABLE opt_out_registry (
    user_id UUID PRIMARY KEY,
    level INT NOT NULL,              -- 1, 2, or 3
    source TEXT NOT NULL,            -- 'cmp', 'platform_page', 'device_setting', 'gdpr_request', 'ccpa_request'
    requested_at TIMESTAMP NOT NULL,
    completed_at TIMESTAMP,          -- NULL until all systems confirm
    verified_at TIMESTAMP,           -- NULL until verification job passes
    systems_completed JSONB          -- {"identity_graph": true, "analytics": true, ...}
);
```

Every service checks this registry (cached in Redis) before processing user data:

```
Any service about to use user_id:
    1. Check Redis: opt_out:{user_id} -> level
    2. If level >= required threshold for this operation -> skip/anonymise
    3. If not in cache -> check Postgres (cache miss, populate cache)
    4. If not in registry -> proceed normally
```

#### SSP/Tracker: Recognising Opted-Out Users

When a user who previously opted out visits a page:

```
User visits publisher page (no platform cookie - deleted at Level 2)
    |
    v
SSP: no cookie -> generate new platform ID? NO.
    Check: did this IP + UA recently opt out? (short-term IP-based blocklist in Redis, 24h TTL)
    |
    +-- Recent opt-out detected -> don't set cookie, serve contextual ad only
    +-- No recent opt-out -> set new cookie (user is treated as new, no history)
```

For Level 1 (personalisation opt-out), the cookie is kept but flagged:
```
Cookie: adtech_uid=uuid-123; adtech_optout=1
SSP reads: user opted out of personalisation -> include in bid request as regs signal
DSP: contextual targeting only for this user
```

#### Verification and Compliance

**Daily verification job** (CronJob) scans for incomplete deletions:

```
For each user in opt_out_registry WHERE level = 3 AND verified_at IS NULL:
    Query every data store for user_id
    If found anywhere -> re-run deletion for that system, alert
    If clean everywhere -> set verified_at = now()
```

**Compliance dashboard:**

- Total opt-out requests by level and source
- Average time to completion (Level 1/2: immediate, Level 3: target < 72 hours)
- Verification pass/fail rate
- Pending deletions queue depth
- Breakdown by regulation (GDPR, CCPA, voluntary)

**Regulatory response:**

If a regulator asks "prove you deleted user X's data":
1. Pull audit log for user X
2. Show: deletion requested at T1, all systems purged at T2, verification passed at T3
3. Show: zero results across all data stores for user X's ID

#### NATS Subjects for Opt-Out

| Subject | Publisher | Subscribers | Payload |
|---|---|---|---|
| `adtech.privacy.opt_out` | Gateway | DSP, Ad Server, Tracker, Reporting, Identity service | OptOutEvent{user_id, level} |
| `adtech.privacy.deletion_requested` | Gateway | Deletion job runner | DeletionEvent{user_id, verification} |
| `adtech.privacy.deletion_completed` | Deletion job | Gateway (dashboard), Audit | DeletionCompletedEvent{user_id, systems_purged} |

#### Implementation

| Component | Location |
|---|---|
| Opt-out API | Gateway: `POST /v1/api/privacy/opt-out`, `POST /v1/api/privacy/delete` |
| Opt-out registry | Postgres `opt_out_registry` table, cached in Redis |
| Opt-out check | `pkg/privacy/optout.go` - `IsOptedOut(userID) -> level` |
| Opt-out propagation | NATS events consumed by all services |
| Deletion job | `cmd/privacy-delete/` - K8s Job, scans and anonymises across all stores |
| Verification job | `cmd/privacy-verify/` - K8s CronJob, daily verification of completed deletions |
| Compliance dashboard | Gateway dashboard - opt-out metrics, pending deletions, verification status |

### 10. Bid Modifiers

The base bid says "this impression is worth $2 CPM." But not all impressions are equal. Mobile users convert better, UK users are higher value, nighttime traffic is cheaper. Bid modifiers let advertisers adjust the bid for specific dimensions without creating separate line items for each combination.

**Available modifier dimensions:**

| Dimension | What it adjusts for | Example |
|---|---|---|
| **Device type** | Different conversion rates by device | mobile: +20%, tablet: -10%, desktop: +0% |
| **OS** | iOS users may have higher LTV | iOS: +15%, Android: +0% |
| **Geo (country)** | Different market values | US: +0%, UK: +15%, IN: -40%, BR: -25% |
| **Geo (region/city)** | Hyperlocal value differences | London: +30%, Manchester: +10% |
| **Time of day** | Peak hours vs off-peak | 09:00-17:00: +10%, 00:00-06:00: -30% |
| **Day of week** | Weekday vs weekend performance | Mon-Fri: +0%, Sat-Sun: -15% |
| **Audience segment** | Known users are more valuable | existing_customers: +50%, in_market: +30% |
| **Inventory type** | Web vs app value difference | site: +0%, app: +10% |
| **Placement position** | Above the fold premium | above_fold: +25%, below_fold: -20% |
| **Connection type** | Wifi users more likely to engage with rich media | wifi: +5%, cellular: -5% |
| **Deal type** | Bid differently for PMP vs open | pmp: +10%, open: +0% |

**How modifiers stack:**

Modifiers are **multiplicative**, not additive. This prevents extreme bids when many modifiers apply:

```
Base bid: $2.00 CPM

Matching modifiers:
    device = mobile:    +20%  -> multiplier 1.20
    geo = UK:           +15%  -> multiplier 1.15
    time = 09:00-17:00: +10%  -> multiplier 1.10
    segment = in_market: +30% -> multiplier 1.30

Modified bid = $2.00 * 1.20 * 1.15 * 1.10 * 1.30 = $3.95

vs additive: $2.00 * (1 + 0.20 + 0.15 + 0.10 + 0.30) = $3.50
```

Multiplicative is safer because each modifier compounds on the modified value, not the base. A -50% modifier on a -50% modifier gives 25% of base (not 0%).

**Modifier bounds (safety rails):**

| Setting | Purpose | Default |
|---|---|---|
| Max positive modifier | Prevent accidentally bidding 10x the base | +200% |
| Max negative modifier | Prevent effectively bidding $0 | -80% |
| Max combined modifier | Cap total adjustment regardless of stacking | +300% / -90% |
| Per-dimension max | Limit any single dimension's impact | Configurable per dimension |

Configured per line item. The platform enforces bounds even if the advertiser sets extreme values:

```
Modified bid = $2.00 * 1.20 * 1.50 * 1.80 * 1.30 = $8.42
Max combined modifier = +300% -> max bid = $2.00 * 4.00 = $8.00
Capped bid: $8.00
```

**Where modifiers fit in the bid calculation pipeline:**

```
1. Base bid value from line item config: $2.00
    |
    v
2. Apply bid modifiers (multiplicative): $3.95
    |
    v
3. Apply bid shading (reduce to efficient price): $2.80
    |
    v
4. Check against line item min/max bid bounds: $2.80 (within bounds)
    |
    v
5. Convert to bid request currency: £2.21 GBP
    |
    v
6. Submit bid to Exchange
```

Modifiers reflect the advertiser's value assessment ("mobile UK users are worth more to me"). Shading then reduces the bid to win at an efficient price. The modifier raises the ceiling, shading finds the right price under that ceiling.

**Modifier performance reporting:**

Reporting shows the impact of each modifier so advertisers can tune them:

| Modifier | Impressions | Spend | CTR | CPA | Recommendation |
|---|---|---|---|---|---|
| mobile +20% | 50,000 | $198 | 1.2% | $4.50 | Performing well, consider +25% |
| tablet -10% | 8,000 | $14 | 0.3% | $12.00 | Underperforming, consider -30% or exclude |
| UK +15% | 30,000 | $135 | 0.9% | $5.00 | On target |
| 09-17 +10% | 40,000 | $160 | 1.0% | $4.80 | Marginal improvement, keep |
| in_market +30% | 15,000 | $78 | 2.1% | $3.20 | Strong performer, consider +40% |

The optimisation pipeline can **auto-suggest modifier adjustments** based on this data: "increase mobile modifier to +25% based on 2-week performance." Advertiser approves or rejects in the dashboard.

**API configuration:**

```json
{
    "line_item_id": "li_123",
    "bid_modifiers": {
        "device": {"mobile": 20, "tablet": -10, "desktop": 0},
        "geo_country": {"US": 0, "UK": 15, "IN": -40},
        "time_of_day": [
            {"start": "09:00", "end": "17:00", "modifier": 10},
            {"start": "00:00", "end": "06:00", "modifier": -30}
        ],
        "audience": {"existing_customers": 50, "in_market": 30},
        "inventory_type": {"app": 10}
    },
    "modifier_bounds": {
        "max_positive": 200,
        "max_negative": -80,
        "max_combined": 300
    }
}
```

Implemented in `pkg/targeting/modifiers.go`. Runs in the DSP during bid evaluation. Modifier configs loaded into L1 cache from Postgres.

### 11. Third-Party Pixel Piggybacking

Advertisers and publishers almost always need to fire their own tracking alongside our platform tracking. An advertiser uses DoubleVerify for brand safety verification. A publisher uses comScore for audience measurement. An agency uses their own attribution system. All of these require additional pixels embedded in the ad markup.

**Types of third-party tracking:**

| Type | What it does | Who configures it | Example |
|---|---|---|---|
| **Impression tracker** | Fires on ad render (1x1 pixel or JS tag) | Advertiser or publisher | DoubleVerify viewability, Nielsen DCR |
| **Click tracker** | Fires on click (redirect chain or JS) | Advertiser | Agency attribution system |
| **Viewability tracker** | Fires when viewability criteria met | Advertiser | MOAT, IAS |
| **Conversion tracker** | Fires on landing page after click | Advertiser | Google Analytics, Facebook pixel |
| **Audience measurement** | Fires for panel-based measurement | Publisher | comScore, Nielsen DAR |
| **Brand safety scanner** | Fires to verify page content is safe | Advertiser | DoubleVerify, IAS pre-bid |

**How piggybacking works:**

```
Ad Server builds ad markup for serving:
    |
    v
1. Platform impression pixel:
    <img src="/v1/t/imp?tid=abc&cid=123&sig=xyz" width="1" height="1">
    |
    v
2. Advertiser third-party pixels (from line item config):
    <img src="https://cdn.doubleverify.com/pixel?ctx=123&cmpId=${CAMPAIGN_ID}&ts=${TIMESTAMP}">
    <img src="https://track.agency.com/imp?campaign=${CAMPAIGN_ID}&cost=${AUCTION_PRICE}">
    |
    v
3. Publisher third-party pixels (from placement config):
    <img src="https://sb.scorecardresearch.com/p?c1=2&c2=12345&ns_site=publisher">
    |
    v
4. Macro substitution runs on ALL URLs (platform and third-party):
    ${CAMPAIGN_ID} -> "camp_123"
    ${AUCTION_PRICE} -> "2.50"
    ${TIMESTAMP} -> "1716825600"
    |
    v
5. Final ad markup sent to browser with all pixels embedded
```

**Configuration at multiple levels:**

| Level | Who sets it | Applies to | Example use case |
|---|---|---|---|
| Advertiser account | Advertiser admin | All ads from this advertiser | Agency attribution pixel on everything |
| Insertion Order | Campaign manager | All line items under this IO | Brand safety verification for this campaign |
| Line Item | Campaign manager | All creatives in this line item | Specific audience panel for this targeting |
| Creative | Creative team | This specific creative | A/B test tracking for this creative variant |
| Publisher account | Publisher admin | All placements for this publisher | comScore audience measurement sitewide |
| Placement | Publisher ad ops | This specific placement | Premium placement with additional verification |

Pixels inherit down the hierarchy and merge (not override). A creative inherits advertiser + IO + line item pixels plus any of its own.

**Pixel types in the ad markup:**

| Pixel type | Implementation | When it fires |
|---|---|---|
| Image pixel (`<img>`) | 1x1 transparent GIF URL | On page load (impression) |
| JavaScript tag (`<script>`) | JS file that executes and may fire additional pixels | On page load |
| Click redirect chain | Click URL wraps through multiple trackers before landing page | On click |
| Event callback (JS) | JS function called by `adtech.js` on viewability/click | On specific event |

**Click redirect chain example:**

When a user clicks an ad with third-party click trackers:

```
User clicks ad
    |
    v
1. Platform click tracker (records click, extracts redirect):
    /v1/t/click?tid=abc&sig=xyz&redir=https%3A%2F%2Ftrack.agency.com%2F...
    |
    v
2. 302 redirect to agency click tracker:
    https://track.agency.com/click?campaign=123&redir=https%3A%2F%2Ftrack.advertiser.com%2F...
    |
    v
3. 302 redirect to advertiser click tracker:
    https://track.advertiser.com/click?src=display&redir=https%3A%2F%2Flanding.advertiser.com
    |
    v
4. Final 302 redirect to landing page:
    https://landing.advertiser.com/shoes?utm_source=display
```

Each tracker in the chain records the click and redirects to the next. The order is: platform first (we must record the click for billing), then third-party trackers, then landing page.

**Privacy interaction:**

Third-party pixels are subject to the same consent rules:

| Privacy level | Third-party pixel behaviour |
|---|---|
| Full consent | All configured third-party pixels fire |
| No personalisation (Level 1) | Measurement pixels fire (comScore, Nielsen). Behavioural pixels suppressed (retargeting pixels). |
| No tracking (Level 2) | Only essential platform pixel fires. All third-party pixels suppressed. |
| COPPA | All third-party pixels suppressed (no external data collection for children) |

The Ad Server checks PrivacyContext before embedding third-party pixels. Each configured pixel is tagged as `measurement` (fires at Level 1) or `behavioural` (suppressed at Level 1+).

**Validation:**

Third-party pixel URLs are validated before saving:

| Check | Why |
|---|---|
| URL is HTTPS | Mixed content blocks HTTP pixels on HTTPS pages |
| URL is reachable | Dead pixels slow page load for no benefit |
| Domain not blocklisted | Prevent malware/redirect hijacking |
| Macro syntax valid | `${INVALID_MACRO}` rejected at save time, not silently dropped at serve time |
| Max pixels per level | Limit total pixels to prevent page bloat (configurable, e.g. max 10 per ad) |

**API configuration:**

```json
{
    "line_item_id": "li_123",
    "third_party_pixels": {
        "impression": [
            {
                "url": "https://cdn.doubleverify.com/pixel?ctx=123&cmpId=${CAMPAIGN_ID}",
                "type": "measurement",
                "vendor": "doubleverify"
            },
            {
                "url": "https://track.agency.com/imp?cost=${AUCTION_PRICE}",
                "type": "behavioural",
                "vendor": "agency_attribution"
            }
        ],
        "click": [
            {
                "url": "https://track.agency.com/click?campaign=${CAMPAIGN_ID}",
                "type": "measurement",
                "vendor": "agency_attribution"
            }
        ]
    }
}
```

**Reporting:**

Third-party pixel fire rates are tracked as Prometheus metrics:
- `adserver_third_party_pixel_fired_total{vendor="doubleverify", type="impression"}`
- `adserver_third_party_pixel_suppressed_total{reason="privacy_level_2"}`
- `adserver_third_party_pixel_error_total{vendor="agency", error="timeout"}`

If a third-party pixel consistently times out, the Ad Server logs a warning but never blocks ad delivery waiting for it. Third-party pixels are fire-and-forget from the Ad Server's perspective.

Implemented in `pkg/adserving/pixels.go`. Pixel configs loaded into L1 cache from Postgres. Macro substitution in `pkg/adserving/macros.go` processes both platform and third-party URLs.

### 12. Reach and Frequency Reporting

Every advertiser asks: "How many people saw my ad, and how many times?" This is fundamentally different from impression counts. 100,000 impressions could mean 100,000 people saw it once, or 10,000 people saw it 10 times each. The business implications are completely different.

**Core metrics:**

| Metric | What it answers | Calculation |
|---|---|---|
| **Reach** | "How many unique users saw my ad?" | Count of distinct user IDs |
| **Frequency** | "How many times did the average user see it?" | Total impressions / reach |
| **Effective reach** | "How many users saw it at least N times?" | Count of users with impressions >= N |
| **Frequency distribution** | "What % saw it 1x, 2x, 3x...?" | Histogram of impressions per user |
| **Incremental reach** | "How many new users did we reach today vs yesterday?" | New unique IDs not seen in prior period |

**The problem: counting unique users at scale**

Exact distinct counting over millions of users requires storing every user ID. For a campaign with 50 million impressions, that's a massive set operation. It gets worse when you need breakdowns by geo, device, creative, and time period - each combination needs its own distinct count.

**Solution: HyperLogLog (HLL)**

HLL is a probabilistic data structure that estimates cardinality (unique count) with ~2% accuracy using only ~12KB of memory regardless of the number of unique elements.

```
Exact distinct count of 10 million users:
    Storage: ~80MB (10M UUIDs)
    Query time: seconds (full scan)

HyperLogLog estimate of 10 million users:
    Storage: 12KB
    Query time: microseconds
    Accuracy: ±2% (reports 9.8M - 10.2M)
```

**How HLL works in our rollups:**

```
Raw events arrive at reporting service:
    impression: {user_id: "abc", campaign_id: "123", geo: "UK", device: "mobile"}
    |
    v
Reporting service maintains HLL sketches per dimension combination:
    hll:campaign:123              -> add("abc")
    hll:campaign:123:geo:UK       -> add("abc")
    hll:campaign:123:device:mobile -> add("abc")
    hll:campaign:123:creative:456  -> add("abc")
    |
    v
Rollups merge HLL sketches:
    Minute HLLs merge into hourly HLLs
    Hourly HLLs merge into daily HLLs
    (HLL merging is lossless - union of two HLLs is exact)
```

**HLL storage in analytics:**

| Rollup level | HLL stored per | Storage per HLL | Example |
|---|---|---|---|
| Minute | campaign + geo + device | 12KB | Campaign 123, UK, mobile, minute 14:32 |
| Hourly | campaign + geo + device | 12KB | Campaign 123, UK, mobile, hour 14:00 |
| Daily | campaign + geo + device + creative | 12KB | Campaign 123, UK, mobile, creative 456, May 27 |
| Monthly | campaign + geo | 12KB | Campaign 123, UK, May 2026 |

HLLs are stored as binary columns in DuckDB/ClickHouse. Both support HLL natively.

**Frequency distribution:**

HLL gives you unique count but not frequency distribution. For "X% saw it 1 time, Y% saw it 2 times," we need per-user impression counts.

Approach: maintain a frequency counter per user per campaign in Redis during the day (same keys as frequency capping: `fc:{user}:{campaign}:d`). At rollup time, read the counters and build a histogram:

```
End of day rollup for campaign 123:
    Read all fc:*:campaign_123:d keys from Redis
    Count: 5000 users saw it 1x, 3000 saw it 2x, 1500 saw it 3x, 500 saw it 4+x
    Store histogram in daily rollup
    |
    v
Frequency distribution:
    1 impression:  50% of reached users
    2 impressions: 30%
    3 impressions: 15%
    4+ impressions: 5%
    Average frequency: 1.75
```

**Cross-dimension reach (overlap analysis):**

"How many users saw Campaign A AND Campaign B?" Requires HLL intersection:

```
HLL for Campaign A: ~100,000 unique users
HLL for Campaign B: ~80,000 unique users
HLL intersection (A ∩ B): ~25,000 users saw both
    |
    v
Campaign A exclusive reach: 75,000
Campaign B exclusive reach: 55,000
Overlap: 25,000
Total unduplicated reach: 155,000
```

HLL supports union (merge) natively. Intersection is estimated via inclusion-exclusion: |A ∩ B| = |A| + |B| - |A ∪ B|.

**Reach curve (saturation):**

Over time, a campaign's reach grows but eventually saturates - you're mostly re-reaching the same users:

```
Day 1:  Reach: 50,000   Frequency: 1.0  (all new users)
Day 3:  Reach: 120,000  Frequency: 1.5  (still finding new users)
Day 7:  Reach: 180,000  Frequency: 2.8  (saturation starting)
Day 14: Reach: 200,000  Frequency: 5.0  (mostly re-reaching)
Day 30: Reach: 210,000  Frequency: 9.5  (heavily saturated)
```

The reach curve helps advertisers decide when to:
- Broaden targeting (reach is saturating, wasting budget on same users)
- Reduce budget (enough people reached)
- End the campaign (diminishing returns)

Surfaced in the campaign dashboard as a chart.

**Reporting API:**

```
GET /v1/api/reports/reach?line_item_id=li_123&from=2026-05-01&to=2026-05-27
Response:
{
    "reach": 210000,
    "impressions": 1995000,
    "average_frequency": 9.5,
    "frequency_distribution": {
        "1": 0.15, "2": 0.20, "3": 0.18, "4": 0.12,
        "5-10": 0.25, "11-20": 0.08, "21+": 0.02
    },
    "daily_reach_curve": [
        {"date": "2026-05-01", "cumulative_reach": 50000, "frequency": 1.0},
        {"date": "2026-05-02", "cumulative_reach": 85000, "frequency": 1.2},
        ...
    ],
    "accuracy": "±2% (HyperLogLog)"
}
```

**Privacy interaction:**

- Users without a platform ID (opted out, no cookie) are not counted in reach. A Prometheus metric tracks "unidentified impressions" so advertisers know their reported reach is a lower bound.
- Cross-device reach uses the identity graph. If the same user is seen on mobile and desktop, they count as one unique user (not two).

Implemented in `pkg/reporting/reach.go`. HLL operations via Go HLL library. Frequency distribution from Redis frequency cap counters repurposed at rollup time.

### 13. View-Through Conversion Attribution

Most conversions don't happen immediately after clicking an ad. A user might see an ad on Monday, think about it, then go directly to the advertiser's website on Wednesday and buy. Without view-through attribution, that conversion is invisible - the advertiser thinks the ad didn't work when it actually did.

**Attribution types:**

| Type | What happened | Example | Typical window |
|---|---|---|---|
| **Click-through** | User clicked the ad, then converted | Click -> landing page -> purchase | 30 days |
| **View-through** | User saw the ad (didn't click), then converted independently | Impression -> (days pass) -> direct visit -> purchase | 7 days |
| **Cross-device** | User saw ad on phone, converted on laptop | Mobile impression -> desktop conversion | Same as above, via identity graph |

**The full attribution flow:**

```
Day 1, 14:00: User "abc" sees ad for Acme Shoes (impression recorded)
    impression: {user_id: "abc", campaign_id: "camp_123", trace_id: "tr_001", timestamp: "Day 1 14:00"}

Day 1, 14:01: User does NOT click. Scrolls past. Ad was viewable for 2.3 seconds.
    viewability: {user_id: "abc", trace_id: "tr_001", duration_ms: 2300, pct: 85}

Day 3, 10:00: User "abc" visits acme.com directly (types URL into browser)

Day 3, 10:05: User purchases shoes. Conversion pixel fires:
    conversion: {user_id: "abc", campaign_id: "camp_123", trace_id: "tr_099",
                 conversion_type: "purchase", value: 89.99}
    |
    v
Attribution engine runs:
    1. Conversion for user "abc", campaign "camp_123"
    2. Look for recent touchpoints for this user + campaign:
       - Click-through: any clicks within 30 days? -> No
       - View-through: any impressions within 7 days? -> Yes! tr_001, Day 1 14:00
    3. Was the impression viewable? -> Yes (2.3s, 85% visible)
    4. Attribution: view-through conversion
       Link: impression tr_001 -> conversion tr_099
       Type: view_through
       Lag: 2 days, 20 hours
```

**Attribution priority (when multiple touchpoints exist):**

A user might see an ad 5 times and click once. Which touchpoint gets credit?

| Model | How it works | Default? |
|---|---|---|
| **Last touch** | Most recent touchpoint before conversion gets 100% credit | Yes (MVP default) |
| **Last click** | Most recent click gets credit. If no click, most recent view. | Common alternative |
| **First touch** | First touchpoint gets credit (who introduced the user to the brand) | No (future) |
| **Linear** | All touchpoints share credit equally | No (future) |
| **Time decay** | Recent touchpoints get more credit than older ones | No (future) |

For MVP, we implement **last touch** (simple, industry standard). The attribution engine finds the most recent qualifying touchpoint.

**Attribution engine processing:**

```
Conversion event arrives at Billing service
    |
    v
1. Look up user_id in impression/click logs (analytics store)
   Query: all impressions/clicks for this user + campaign within attribution windows
    |
    v
2. Sort touchpoints by timestamp (most recent first)
    |
    v
3. Apply priority:
   a. Any clicks within click-through window (30 days)? -> credit to last click
   b. No clicks? Any viewable impressions within view-through window (7 days)? -> credit to last view
   c. No viewable impressions? Any impressions within window? -> credit to last impression
   d. Nothing? -> organic conversion, no attribution (not billed)
    |
    v
4. Record attribution:
   {
       conversion_id: "conv_099",
       attributed_to: "imp_001",      // the touchpoint that gets credit
       attribution_type: "view_through",
       attribution_lag_hours: 68,
       conversion_value: 89.99,
       touchpoint_chain: ["imp_001 (Day 1)", "imp_002 (Day 2)", "imp_003 (Day 3)"]
   }
    |
    v
5. Billing:
   - CPA campaign? Settle the budget reservation using conversion value
   - CPM/CPC campaign? No billing impact, but conversion counted for ROAS reporting
```

**Attribution windows (configurable per line item):**

```json
{
    "line_item_id": "li_123",
    "attribution": {
        "model": "last_touch",
        "click_through_window_days": 30,
        "view_through_window_days": 7,
        "require_viewability": true,
        "min_viewability_seconds": 1.0,
        "cross_device": true
    }
}
```

| Setting | What it controls | Default |
|---|---|---|
| `model` | Attribution model | `last_touch` |
| `click_through_window_days` | How long after a click a conversion can be attributed | 30 |
| `view_through_window_days` | How long after a view a conversion can be attributed | 7 |
| `require_viewability` | Only count viewable impressions for view-through | true |
| `min_viewability_seconds` | Minimum view duration to qualify | 1.0 |
| `cross_device` | Match conversions across devices via identity graph | true |

**Cross-device attribution:**

```
Day 1: User sees ad on mobile (user_id: "mob_123")
Day 3: User converts on desktop (user_id: "desk_456")
    |
    v
Identity graph lookup:
    "mob_123" and "desk_456" linked via same hashed_email
    -> Same person
    |
    v
Attribution: mobile impression -> desktop conversion (cross-device view-through)
```

Without cross-device attribution, this conversion would be unattributed. The identity graph is critical for accurate measurement.

**Conversion deduplication:**

The same conversion must not be counted twice:

| Scenario | Handling |
|---|---|
| Same conversion pixel fires twice (network retry) | Deduplicate by conversion_id (hash of user + campaign + conversion_type + timestamp window) |
| User converts on multiple campaigns from same advertiser | Each campaign gets independent attribution (both campaigns contributed) |
| User converts, then returns/refunds | Advertiser can upload negative conversions to reverse attribution |

**Reporting:**

| Metric | What it shows |
|---|---|
| Total conversions | Click-through + view-through combined |
| Click-through conversions | Conversions attributed to a click |
| View-through conversions | Conversions attributed to a view (no click) |
| View-through rate | View-through conversions / total conversions |
| Average attribution lag | Mean time between touchpoint and conversion |
| Conversion path length | Average number of touchpoints before conversion |
| ROAS (by attribution type) | Revenue / spend, broken down by click-through vs view-through |

Advertisers can see: "60% of our conversions are view-through, with an average lag of 2.5 days. View-through ROAS is 3.2x vs click-through ROAS of 5.1x."

**Privacy interaction:**

- View-through attribution requires user identity (platform ID). Opted-out users cannot have conversions attributed to impressions.
- GDPR: attribution is a "legitimate interest" processing purpose when the user has consented to tracking. Without consent, no attribution.
- Attribution data is subject to the same deletion rules as other user data (Level 3 opt-out anonymises attribution records).

Implemented in `pkg/billing/attribution.go`. Runs in the Billing service when conversion events arrive from NATS. Uses analytics store to look up historical touchpoints. Attribution records stored in Postgres for reporting.

### 14. Competitive Separation

Coca-Cola doesn't want their ad next to Pepsi's. A BMW ad next to an Audi ad makes both brands look cheaper. Competitive separation prevents competitors from appearing together on the same page, protecting brand perception and satisfying advertiser requirements.

**Separation levels:**

| Level | What it prevents | Example | Enforced by |
|---|---|---|---|
| **Direct competitor** | Named competitor on same page | Coca-Cola and Pepsi | Exchange |
| **Industry category** | Same IAB industry category on same page | Any two car brands | Exchange or Publisher |
| **Advertiser self-separation** | Same advertiser's different campaigns on same page | Two Acme campaigns competing against each other | Exchange |
| **Publisher-enforced** | Publisher's own rules about which categories can coexist | "No two financial ads on homepage" | Publisher quality controls |

**How it works - the page request:**

A publisher page with multiple ad slots sends all slots in one request (or the SSP groups them by page):

```
User loads nytimes.com/sports
Page has 3 ad slots: header (728x90), sidebar (300x250), footer (300x250)
    |
    v
SSP sends 3 bid requests to Exchange with same page_request_id: "page_abc"

Exchange processes sequentially (or with awareness of parallel results):
```

**Auction flow with competitive separation:**

```
Page request "page_abc" has 3 slots:

Slot 1 (header) auction:
    Bids: Coca-Cola $5.00, Nike $4.50, BMW $4.00
    Winner: Coca-Cola
    Exchange records: page_abc -> [{advertiser: "coca-cola", category: "beverages"}]

Slot 2 (sidebar) auction:
    Bids: Pepsi $4.80, Nike $4.50, Acme Sports $3.00
    |
    v
    Competitive separation check for each bid:
        Pepsi: category "beverages" -> CONFLICT with Coca-Cola on slot 1 -> BLOCKED
        Pepsi: direct competitor of Coca-Cola -> DOUBLE BLOCKED
        Nike: category "sportswear" -> no conflict -> ALLOWED
        Acme Sports: category "sportswear" -> no conflict -> ALLOWED
    |
    v
    Eligible bids: Nike $4.50, Acme Sports $3.00
    Winner: Nike
    Exchange records: page_abc -> [{coca-cola, beverages}, {nike, sportswear}]

Slot 3 (footer) auction:
    Bids: Adidas $3.50, BMW $3.00, Acme Sports $2.50
    |
    v
    Competitive separation check:
        Adidas: category "sportswear" -> CONFLICT with Nike on slot 2 -> BLOCKED
        BMW: category "automotive" -> no conflict -> ALLOWED
        Acme Sports: category "sportswear" -> CONFLICT with Nike -> BLOCKED
    |
    v
    Eligible bids: BMW $3.00
    Winner: BMW
```

Result: Coca-Cola, Nike, BMW - three different categories, no competitors on the page.

**Advertiser competitive configuration:**

```json
{
    "advertiser_id": "adv_coca_cola",
    "industry_category": "IAB8-1",
    "competitive_separation": {
        "mode": "category_and_direct",
        "direct_competitors": ["adv_pepsi", "adv_dr_pepper", "adv_red_bull"],
        "blocked_categories": ["IAB8-1"],
        "self_separation": true
    }
}
```

| Setting | What it does |
|---|---|
| `mode: "category_and_direct"` | Block both same-category and named competitors |
| `mode: "direct_only"` | Only block named competitors (different beverage brands OK) |
| `mode: "category_only"` | Block same category (even non-competitors) |
| `mode: "none"` | No competitive separation |
| `direct_competitors` | Named advertiser IDs to never co-appear with |
| `blocked_categories` | IAB categories to separate from |
| `self_separation` | Prevent own campaigns from competing against each other on same page |

**Self-separation detail:**

An advertiser running 3 campaigns doesn't want them competing against each other on the same page - that wastes budget (bidding against yourself) and looks bad (3 Acme ads on one page):

```
Without self-separation:
    Slot 1: Acme Campaign A wins at $5.00
    Slot 2: Acme Campaign B wins at $4.80 (bidding against yourself)
    Slot 3: Acme Campaign C wins at $4.50 (3 Acme ads on page = ad fatigue)

With self-separation:
    Slot 1: Acme Campaign A wins at $5.00
    Slot 2: Acme Campaigns B & C blocked. Nike wins at $4.50
    Slot 3: Acme Campaigns B & C blocked. BMW wins at $3.00
```

**Publisher-side separation rules:**

Publishers can also enforce separation independently:

```json
{
    "placement_id": "pl_homepage_header",
    "separation_rules": {
        "max_same_category_per_page": 1,
        "blocked_category_pairs": [
            ["IAB8-1", "IAB8-2"]
        ],
        "max_same_advertiser_per_page": 1
    }
}
```

**Exchange implementation:**

The Exchange maintains a short-lived **page context** in Redis:

```
Key: page_context:{page_request_id}
Value: [
    {advertiser_id: "coca-cola", categories: ["IAB8-1"], slot: 1},
    {advertiser_id: "nike", categories: ["IAB18"], slot: 2}
]
TTL: 30 seconds (page context expires quickly - only needed during page load)
```

For each bid in a slot auction, the Exchange checks this context:
1. Is this advertiser already on the page? (self-separation)
2. Is this advertiser a named competitor of anyone on the page? (direct)
3. Is this advertiser's category already on the page? (category)
4. Does this violate any publisher separation rules?

If any check fails, the bid is excluded from this slot's auction. The DSP receives a loss notification with `reason: 103` (blocked by publisher controls).

**Impact on revenue:**

Competitive separation reduces competition in each auction (fewer eligible bids), which can lower clearing prices. This is a trade-off:

| Concern | Effect |
|---|---|
| Advertiser satisfaction | Higher - brand protection, no competitor adjacency |
| Publisher revenue per slot | Potentially lower - fewer bidders per auction |
| Publisher total revenue | Often neutral - blocked bids go to other pages/slots |
| Fill rate | Slightly lower if separation removes all eligible bids for a slot |

If separation removes ALL bids for a slot, the Ad Server serves a default/fallback ad. This is tracked as a metric: `exchange_separation_blocked_total{reason="category"}` and surfaced in publisher reporting so they can tune their rules.

**Reporting:**

- Advertiser: "X bids were blocked due to competitive separation this week" (shows volume impact)
- Publisher: "X% of auctions had bids removed by separation rules" with revenue impact estimate
- Exchange: separation block rate by category, by advertiser pair, by page

Implemented in `pkg/auction/separation.go`. Page context stored in Redis with short TTL. Advertiser competitive config loaded into L1 cache from Postgres.

### 15. Macro Substitution in Creatives

Ad creatives and third-party tracking URLs contain placeholder macros like `${AUCTION_PRICE}` that get replaced with real values at the moment the ad is served. This is how dynamic information (price paid, timestamps, IDs) flows into static creative templates and external tracking systems.

**Available macros:**

| Macro | Replaced with | Example value | Used by |
|---|---|---|---|
| `${AUCTION_PRICE}` | Clearing price in bid currency | `2.50` | Third-party cost tracking |
| `${AUCTION_PRICE_ENC}` | Encrypted clearing price | `a1b2c3d4e5...` | Prevent price snooping in URL |
| `${CLICK_URL}` | Platform click tracking URL | `https://.../v1/t/click?tid=abc&...` | Wrapping advertiser landing page |
| `${CLICK_URL_ENC}` | URL-encoded click tracking URL | `https%3A%2F%2F...` | Nested in third-party redirect chains |
| `${CLICK_URL_DBL_ENC}` | Double URL-encoded click URL | `https%253A%252F%252F...` | Deep nesting (tracker within tracker) |
| `${TIMESTAMP}` | Unix timestamp at serve time | `1716825600` | Cache-busting, timing |
| `${AUCTION_ID}` | Trace ID / auction ID | `abc-123-def` | Cross-system event correlation |
| `${CREATIVE_ID}` | Creative identifier | `cr_456` | Creative-level tracking |
| `${CAMPAIGN_ID}` | Line item identifier | `li_789` | Campaign-level tracking |
| `${LINE_ITEM_ID}` | Line item identifier (alias) | `li_789` | Same as CAMPAIGN_ID |
| `${IO_ID}` | Insertion order identifier | `io_012` | IO-level tracking |
| `${ADVERTISER_ID}` | Advertiser identifier | `adv_345` | Advertiser-level tracking |
| `${PLACEMENT_ID}` | Placement identifier | `pl_678` | Placement-level tracking |
| `${PUBLISHER_ID}` | Publisher identifier | `pub_901` | Publisher-level tracking |
| `${SITE_DOMAIN}` | Publisher domain | `nytimes.com` | Contextual reporting |
| `${APP_BUNDLE}` | App bundle ID (mobile) | `com.nytimes.ios` | App tracking |
| `${USER_AGENT}` | URL-encoded user agent | `Mozilla%2F5.0...` | Device detection |
| `${IP}` | User's IP (hashed if privacy required) | `203.0.113.42` | Geo verification |
| `${WIDTH}` | Ad slot width in pixels | `300` | Responsive creative |
| `${HEIGHT}` | Ad slot height in pixels | `250` | Responsive creative |
| `${CURRENCY}` | Bid currency ISO code | `GBP` | Multi-currency tracking |
| `${DEAL_ID}` | Deal identifier (if applicable) | `deal_555` | Deal-level reporting |
| `${CACHEBUSTER}` | Random number (prevents browser caching) | `8374629153` | Ensures pixel fires every time |

**How substitution works:**

```
Creative HTML stored in object storage (Minio/S3):

<div class="ad">
    <a href="${CLICK_URL}https://landing.acme.com/shoes?utm_campaign=${CAMPAIGN_ID}">
        <img src="https://cdn.acme.com/banner.jpg" width="${WIDTH}" height="${HEIGHT}">
    </a>
    <img src="https://track.agency.com/imp?price=${AUCTION_PRICE}&ts=${TIMESTAMP}&cb=${CACHEBUSTER}" width="1" height="1">
    <img src="/v1/t/imp?tid=${AUCTION_ID}&cid=${CAMPAIGN_ID}&pid=${PLACEMENT_ID}&sig=abc" width="1" height="1">
</div>

    |
    v

Ad Server substitutes all macros at serve time:

<div class="ad">
    <a href="https://tracker.example.com/v1/t/click?tid=abc-123&redir=https%3A%2F%2Flanding.acme.com%2Fshoes%3Futm_campaign%3Dli_789">
        <img src="https://cdn.acme.com/banner.jpg" width="300" height="250">
    </a>
    <img src="https://track.agency.com/imp?price=2.50&ts=1716825600&cb=8374629153" width="1" height="1">
    <img src="/v1/t/imp?tid=abc-123&cid=li_789&pid=pl_678&sig=abc" width="1" height="1">
</div>
```

**Encrypted auction price:**

`${AUCTION_PRICE}` exposes the clearing price in plain text in the URL. This is visible to anyone inspecting network traffic. For sensitive use cases, `${AUCTION_PRICE_ENC}` provides an encrypted version:

```
Encryption: AES-256-GCM with a shared key between platform and advertiser
Decryption: advertiser's server decrypts to get the real price
Format: base64url-encoded ciphertext

${AUCTION_PRICE}     -> "2.50"
${AUCTION_PRICE_ENC} -> "a1b2c3d4e5f6..."  (advertiser decrypts server-side)
```

The encryption key is provisioned per advertiser account and exchanged during onboarding. Stored in K8s secrets.

**Click URL wrapping:**

`${CLICK_URL}` is special - it wraps the advertiser's landing URL so the platform records the click:

```
Creative configured with landing URL: https://landing.acme.com/shoes

At serve time:
${CLICK_URL} -> https://tracker.example.com/v1/t/click?tid=abc&sig=xyz&redir=https%3A%2F%2Flanding.acme.com%2Fshoes

User clicks:
1. Browser hits our click tracker
2. We record the click event
3. 302 redirect to landing.acme.com/shoes
```

For third-party click trackers in the redirect chain, `${CLICK_URL_ENC}` is used because the platform URL is nested inside another URL:

```
Third-party tracker: https://track.agency.com/click?redir=${CLICK_URL_ENC}
After substitution: https://track.agency.com/click?redir=https%3A%2F%2Ftracker.example.com%2Fv1%2Ft%2Fclick%3Ftid%3Dabc...
```

**Validation at creative upload:**

| Check | What it catches |
|---|---|
| Unknown macro | `${INVALID_MACRO}` -> rejected at upload, not silently left in |
| Unbalanced braces | `${AUCTION_PRICE` missing closing brace -> rejected |
| Macro in wrong context | `${AUCTION_PRICE}` in image `src` attribute (should use `${AUCTION_PRICE_ENC}`) -> warning |
| Missing required macros | Click-wrapped creative must contain `${CLICK_URL}` -> rejected if missing |

Validation runs during creative upload (`POST /v1/api/creatives`). Errors returned immediately so the advertiser can fix before submitting for review.

**Performance:**

Macro substitution runs on every ad serve. It must be fast:
- Regex pre-compiled at service startup, not per-request
- Macro values pre-calculated into a map once per request, then string replacement is a single pass
- Total substitution time target: < 0.5ms per creative

Implemented in `pkg/adserving/macros.go`. The Ad Server processes both platform and third-party URLs in the same pass.

### 16. Variable Margin and Revenue Share

Every publisher has a different contract. A large publisher with premium inventory negotiates a lower platform fee. A small publisher on standard terms pays more. Deal types have different margins. The billing system must handle all of these transparently.

**Revenue share models:**

| Model | How it works | When used |
|---|---|---|
| **Fixed percentage** | Platform takes X% of clearing price. Simple and predictable. | Standard publisher contracts |
| **Tiered percentage** | Rate decreases as volume increases. Rewards growth. | Medium-large publishers |
| **Guaranteed minimum** | Publisher earns at least $X CPM regardless of clearing price. Platform absorbs the difference if auction clears below minimum. | Premium publishers with guaranteed revenue |
| **Deal-type based** | Different take rate for each deal type. PG has lower margin (guaranteed volume = less risk for publisher). | Publishers with mixed deal types |
| **Hybrid** | Combination of the above. Tiered base rate + deal-type modifier + guaranteed minimum floor. | Enterprise publisher contracts |

**How each model calculates publisher revenue:**

**Fixed percentage:**
```
Clearing price: $3.00 CPM
Platform fee: 20%
Publisher revenue: $3.00 * (1 - 0.20) = $2.40
Platform margin: $0.60
```

**Tiered percentage:**
```
Publisher monthly impressions so far: 5,000,000 (in 1M-10M tier)

Tier structure:
    0 - 1M:     25% platform fee
    1M - 10M:   20% platform fee
    10M - 50M:  15% platform fee
    50M+:       12% platform fee

Current tier: 20%
Clearing price: $3.00 CPM
Publisher revenue: $3.00 * (1 - 0.20) = $2.40

Next month if publisher hits 12M impressions:
    First 1M at 25%:   publisher gets 75%
    Next 9M at 20%:    publisher gets 80%
    Next 2M at 15%:    publisher gets 85%
    (progressive tiers, not retroactive - like tax brackets)
```

**Guaranteed minimum:**
```
Contract: publisher guaranteed minimum $1.50 CPM

Scenario A - clearing price $3.00 (above minimum):
    Publisher revenue: $3.00 * (1 - 0.20) = $2.40 (normal calculation)
    Platform margin: $0.60

Scenario B - clearing price $0.80 (below minimum):
    Publisher revenue: $1.50 (guaranteed minimum)
    Platform margin: $0.80 - $1.50 = -$0.70 (platform LOSES money on this impression)

Scenario C - no fill (no winning bid):
    Publisher revenue: $0 (guaranteed minimum only applies to filled impressions,
                          unless contract specifies guaranteed fill - rare)
```

Guaranteed minimums are risky for the platform. The billing service tracks "subsidy" separately:

```
publisher_revenue = max(clearing_price * (1 - fee), guaranteed_minimum)
platform_margin = clearing_price - publisher_revenue
subsidy = max(0, guaranteed_minimum - clearing_price * (1 - fee))
```

If a publisher consistently clears below their minimum, the platform is subsidising them. The finance dashboard flags this.

**Deal-type based:**
```
Publisher contract:
    Open auction: 25% platform fee
    PMP: 15% platform fee (publisher curated the audience)
    PG: 10% platform fee (guaranteed volume, low risk)
    Preferred deal: 12% platform fee

Same publisher, same day:
    Open auction impression at $2.00 -> publisher gets $1.50 (25% fee)
    PMP impression at $4.00         -> publisher gets $3.40 (15% fee)
    PG impression at $3.00          -> publisher gets $2.70 (10% fee)
```

**Hybrid (enterprise contracts):**
```
Publisher contract:
    Base: tiered percentage (volume-based)
    Modifier: deal-type discounts
    Floor: guaranteed minimum $1.00 CPM

Calculation:
    1. Determine base tier fee from monthly volume: 20%
    2. Apply deal-type modifier: PMP gets -5% discount -> 15%
    3. Calculate revenue: $4.00 * (1 - 0.15) = $3.40
    4. Check guaranteed minimum: $3.40 > $1.00 -> no subsidy
    Publisher revenue: $3.40
```

**Contract configuration:**

```json
{
    "publisher_id": "pub_123",
    "contract": {
        "model": "hybrid",
        "base_model": "tiered",
        "tiers": [
            {"min_impressions": 0, "max_impressions": 1000000, "fee_pct": 25},
            {"min_impressions": 1000000, "max_impressions": 10000000, "fee_pct": 20},
            {"min_impressions": 10000000, "max_impressions": 50000000, "fee_pct": 15},
            {"min_impressions": 50000000, "max_impressions": null, "fee_pct": 12}
        ],
        "deal_type_modifiers": {
            "open": 0,
            "pmp": -5,
            "pg": -10,
            "preferred": -8
        },
        "guaranteed_minimum_cpm": 1.00,
        "currency": "USD",
        "effective_date": "2026-01-01",
        "end_date": "2026-12-31",
        "payment_terms": "net_30"
    }
}
```

**Billing service calculation flow:**

```
AuctionWinEvent arrives:
    clearing_price: $4.00, publisher_id: "pub_123", deal_type: "pmp"
    |
    v
1. Load publisher contract from cache (L1, invalidated on change)
    |
    v
2. Determine current tier:
    Query: publisher's month-to-date impressions -> 5.2M -> tier 2 (20% fee)
    |
    v
3. Apply deal-type modifier:
    Base fee: 20%, PMP modifier: -5% -> effective fee: 15%
    |
    v
4. Calculate publisher revenue:
    $4.00 * (1 - 0.15) = $3.40
    |
    v
5. Check guaranteed minimum:
    $3.40 > $1.00 -> no subsidy needed
    |
    v
6. Record:
    advertiser_spend:   $4.00
    publisher_revenue:  $3.40
    platform_margin:    $0.60
    fee_pct_applied:    15%
    tier:               2
    subsidy:            $0.00
```

**Tier progression tracking:**

The billing service tracks month-to-date impressions per publisher. When a publisher crosses a tier boundary, the new rate applies to subsequent impressions (progressive, not retroactive):

```
Publisher month-to-date: 990,000 impressions (tier 1: 25%)
Next 10,000 impressions -> cross 1M boundary
    First 10,000 at 25% (tier 1)
    Remaining impressions this month at 20% (tier 2)
```

A NATS event `adtech.billing.tier_changed` is published when a publisher crosses a tier. The publisher dashboard shows current tier, progress to next tier, and projected tier for the month.

**Contract management:**

| Concern | Approach |
|---|---|
| Contract versioning | Each contract has effective/end dates. Historical payouts use the contract that was active at the time. |
| Contract changes | New contract created with future effective date. Old contract stays for historical billing. Both in audit log. |
| Retroactive adjustments | If a contract is corrected retroactively (rare), the billing service recalculates affected payouts as adjustments. |
| Multiple contracts | A publisher could have different contracts for different placements (e.g., premium homepage vs standard ROS). Configured per placement group. |

**Finance reporting:**

| Report | What it shows |
|---|---|
| Publisher payout detail | Revenue per impression type, tier applied, deal-type breakdown, subsidies |
| Platform margin report | Margin by publisher, by deal type, by tier. Identifies low/negative margin publishers. |
| Subsidy report | Publishers where guaranteed minimum exceeds market clearing price. Alerts if subsidy grows. |
| Tier progression | Publishers approaching next tier, projected tier crossings, revenue impact |
| Contract comparison | Side-by-side comparison of contract terms across publishers (normalised) |

**API endpoints:**

- `GET /v1/api/publishers/{id}/contract` - current contract terms
- `PUT /v1/api/publishers/{id}/contract` - update contract (platform admin, audit logged)
- `GET /v1/api/publishers/{id}/contract/history` - contract version history
- `GET /v1/api/publishers/{id}/tier-status` - current tier, month-to-date volume, next tier threshold

Implemented in `pkg/billing/revshare.go`. Contract configs loaded into L1 cache. Tier progression tracked in Redis (month-to-date impression counter per publisher) and flushed to Postgres.

### Implementation

| Component | Location |
|---|---|
| Bid shading | `pkg/auction/shading.go` - real-time bid reduction model |
| Campaign hierarchy | `pkg/models/` - InsertionOrder, LineItem models; budget inheritance logic |
| Frequency cap checks | `cmd/adserver/freqcap.go` - per-user + per-household + advertiser-rule Redis checks (all formats) |
| Targeting exclusions | `pkg/targeting/` - exclusion evaluation after inclusion matching |
| Multi-currency | `pkg/currency/` - conversion, rate storage, daily update job |
| Native ad format | `pkg/openrtb/native.go` - native request/response types |
| OpenRTB app + regs | `pkg/openrtb/` - app object, regs object with consent fields |
| Bid modifiers | `pkg/targeting/modifiers.go` - multiplicative modifier application |
| Third-party pixels | `pkg/adserving/pixels.go` - pixel piggybacking, macro substitution |
| Macro substitution | `pkg/adserving/macros.go` - `${...}` pattern replacement at serve time |
| Reach/frequency | `pkg/reporting/reach.go` - HyperLogLog unique counting |
| View-through attribution | `pkg/billing/attribution.go` - window-based impression-to-conversion matching |
| Competitive separation | `pkg/auction/separation.go` - per-page competitor tracking |
| Revenue share models | `pkg/billing/revshare.go` - fixed, tiered, guaranteed, per-deal-type |

---

## DSP Core Mechanics

### 17. Pacing Algorithms

Pacing controls **how fast** a campaign spends its budget throughout the day. Without it, a campaign burns through its daily budget by 10am and misses the rest of the day's inventory.

#### Pacing Modes

| Mode | Behaviour | When to use |
|---|---|---|
| **Even** (default) | Spread spend smoothly across the day. At 3pm (62.5% of day), ~62.5% of budget should be spent. | Standard campaigns, consistent exposure |
| **ASAP** | Spend as fast as possible. Bid on every eligible request until budget is gone. | Flash sales, time-sensitive promotions, remaining budget end of flight |
| **Front-loaded** | Spend more in early hours, taper off. 80% spent by noon, remaining 20% in afternoon. | Launch campaigns, maximum early reach |
| **Custom curve** | Advertiser-defined hourly spend percentages. | Campaigns aligned with specific daily patterns |

#### The Pacing Algorithm (PID Controller)

The pacer runs on **every bid request** in the DSP. It decides: should we bid on this request, or skip it to preserve budget for later?

```
Bid request arrives at DSP:
    |
    v
1. Calculate target spend at this moment:
    daily_budget = $100
    time_elapsed = 62.5% of day (3pm)
    pacing_mode = "even"
    target_spend = $100 * 0.625 = $62.50
    |
    v
2. Get actual spend so far:
    actual_spend = Redis GET dsp:budget:{campaign_id}
    initial_budget - current_balance = $58.00 spent
    |
    v
3. Calculate pacing ratio:
    pacing_ratio = actual_spend / target_spend = $58.00 / $62.50 = 0.928
    |
    v
4. Decide bid/no-bid:
    ratio < 0.8:  BEHIND PACE - bid aggressively (bid on 100% of eligible requests)
    ratio 0.8-1.0: SLIGHTLY BEHIND - bid normally (bid on 90% of eligible requests)
    ratio 1.0-1.2: ON PACE - bid selectively (bid on 70% of eligible requests)
    ratio > 1.2:  AHEAD OF PACE - throttle (bid on 30% of eligible requests)
    ratio > 1.5:  FAR AHEAD - heavy throttle (bid on 10% of eligible requests)
    |
    v
5. If bidding: proceed to targeting evaluation, bid calculation, shading
   If throttled: skip this request (no-bid), save budget for later
```

**Probabilistic throttling:** When the pacer says "bid on 70% of requests," it uses a random number: `if rand.Float64() < 0.70 { bid } else { skip }`. This distributes the throttling randomly across all requests rather than bursting.

#### Pacing Feedback Loop

The pacer adjusts every **60 seconds** based on actual vs target spend:

```
Every 60 seconds:
    |
    v
    actual_spend_rate = spend_in_last_60s
    target_spend_rate = remaining_budget / remaining_seconds_in_day
    |
    v
    if actual_rate > target_rate * 1.2:
        decrease throttle_pct by 10% (bid on fewer requests)
    if actual_rate < target_rate * 0.8:
        increase throttle_pct by 10% (bid on more requests)
    |
    v
    Store throttle_pct in Redis: dsp:pacing:{campaign_id} = 0.70
    (hot path reads this on every bid request)
```

#### Interaction with Bid Shading and Modifiers

```
Bid request arrives
    |
    v
1. PACING CHECK: should we bid at all? (throttle_pct check)
    +-- Throttled -> no-bid (skip everything, save budget)
    +-- Not throttled -> continue
    |
    v
2. TARGETING: does this request match? (inclusions/exclusions)
    |
    v
3. BASE BID: from line item config ($2.00 CPM)
    |
    v
4. BID MODIFIERS: adjust for device/geo/time/audience ($2.40)
    |
    v
5. BID SHADING: reduce to efficient price ($1.80)
    |
    v
6. Submit bid
```

Pacing runs FIRST. If the pacer says no-bid, nothing else runs. This saves compute.

#### Multi-Line-Item Pacing Within an IO

When multiple line items share an IO budget pool:

```
IO daily budget: $500
    |
    +-- Line Item A: no sub-budget, pacing even
    +-- Line Item B: sub-budget $150, pacing even
    +-- Line Item C: no sub-budget, pacing ASAP
    |
    v
IO pacer distributes:
    Line Item B: capped at $150/day, paces independently
    Line Items A & C share remaining $350/day
    Line Item C (ASAP): gets priority, spends until its targets are met
    Line Item A (even): gets remainder, paces evenly
    |
    v
If Line Item B underspends (only used $100):
    Surplus $50 redistributed to A and C for the rest of the day
```

#### Budget Rollover

| Setting | Behaviour |
|---|---|
| `rollover: false` (default) | Unspent daily budget is lost. $80 spent of $100 = $20 wasted. |
| `rollover: true` | Unspent daily budget adds to next day. $80 spent of $100 = $120 budget tomorrow. |
| `rollover_cap: 150%` | Rollover capped at 150% of daily budget. Max $150 on any day. |

Implemented in `pkg/pacing/`. Throttle percentages stored in Redis. Feedback loop runs in the DSP as a background goroutine.

### 18. Campaign Flight Management

The operational mechanics of starting, stopping, and managing campaigns across time boundaries.

#### Day Boundary Processing

A CronJob (`cmd/reporting --mode=day-boundary`) runs at **midnight UTC** and handles:

```
Midnight UTC:
    |
    v
1. DAILY BUDGET RESET:
    For each active line item with daily budget:
        - Snapshot today's spend to Postgres (daily_spend_snapshots table)
        - Calculate rollover (if enabled): tomorrow_budget = daily_budget + unspent
        - Reset Redis budget counter: dsp:budget:{campaign_id}:daily = new_daily_budget
        - Reset Redis pacing throttle: dsp:pacing:{campaign_id} = 1.0 (full speed)
    |
    v
2. FLIGHT DATE CHECK:
    For each line item where start_date = today:
        - Transition state: approved -> live
        - Load targeting/budget into Redis cache
        - Publish NATS: adtech.campaign.state_changed {state: "live"}
    For each line item where end_date = yesterday:
        - Transition state: live -> ended
        - Remove from Redis cache
        - Publish NATS: adtech.campaign.state_changed {state: "ended"}
    |
    v
3. IO BUDGET CHECK:
    For each IO where total spend >= total budget:
        - Pause all line items under this IO
        - Publish NATS: adtech.budget.depleted {io_id}
    For each IO where end_date = yesterday:
        - End all line items under this IO
    |
    v
4. FREQUENCY CAP RESET:
    Daily frequency caps in Redis: TTL handles this automatically (24h keys expire)
    Weekly caps: checked and reset by this job if using fixed-week windows
```

#### Timezone-Aware Daily Budgets

Advertisers set budgets in their timezone. The platform runs on UTC internally.

```
Advertiser in US Pacific (UTC-7):
    Their "day" = 07:00 UTC to 07:00 UTC next day

How it works:
    1. Line item stores: timezone = "America/Los_Angeles"
    2. Day boundary job runs at midnight UTC
    3. For this line item: midnight Pacific = 07:00 UTC
    4. Job schedules a delayed reset at 07:00 UTC for this campaign
    |
    v
    Implementation: day boundary job runs hourly (not just midnight UTC)
    Each run: "which campaigns have a day boundary NOW in their timezone?"

    00:00 UTC run: reset campaigns in UTC+0 (UK)
    01:00 UTC run: reset campaigns in UTC+1 (CET)
    ...
    07:00 UTC run: reset campaigns in UTC-7 (US Pacific)
    ...
```

Alternative simpler approach: all daily budgets operate on UTC day. Advertiser is told "daily budget resets at midnight UTC." Most platforms do this.

Configuration: live config `campaigns.daily_budget_timezone = "utc"` (simple) or `"advertiser"` (complex). Default: UTC.

#### What Happens If the Day Boundary Job Fails

| Failure | Impact | Recovery |
|---|---|---|
| Job doesn't run | Daily budgets not reset. Campaigns that hit yesterday's limit stay stopped. | Alert P2. Manual run. Campaigns resume when job catches up. |
| Job runs but Redis write fails | Some campaigns not reset. | Job retries failed campaigns. Idempotent (safe to re-run). |
| Job runs but Postgres snapshot fails | Spend data not archived. | Retry. Spend data reconstructable from NATS events. |

The job is **idempotent** - running it twice for the same day boundary is safe (checks if already processed).

### 19. Contextual Targeting Classification Engine

How pages get classified into targetable categories when there's no user data (no cookie, no consent).

#### Classification Methods

| Method | How | Latency | Accuracy | When used |
|---|---|---|---|---|
| **Publisher-declared** | Publisher tags their pages with IAB categories in the ad tag | 0ms (in bid request) | High (publisher knows their content) | Default, all publishers |
| **URL pattern matching** | `/sports/*` = IAB17, `/finance/*` = IAB12 | 0ms (regex match) | Medium (URL doesn't always reflect content) | Supplement publisher declarations |
| **Pre-crawled classification** | Crawler visits publisher pages, classifies via ML, stores results | 0ms (lookup from cache) | High (actual content analysed) | Premium publishers, high-value inventory |
| **Real-time keyword extraction** | Publisher sends page keywords in ad tag | 0ms (in bid request) | Medium-High | Publishers that integrate keyword signals |

#### Publisher-Declared Categories (Primary Method)

The simplest and fastest. Publisher configures categories per placement or sends them via the ad tag:

```javascript
// Publisher's ad tag sends page-level context
adtech.setPageContext({
    categories: ["IAB17-1", "IAB17-12"],   // Sports > Soccer, Sports > Tennis
    keywords: ["premier league", "match report", "chelsea"],
    content_rating: "G",
    article_sentiment: "positive"
});
```

This data flows into the bid request:

```json
{
    "site": {
        "domain": "sportsmedia.com",
        "page": "https://sportsmedia.com/football/match-report",
        "cat": ["IAB17-1", "IAB17-12"],
        "content": {
            "keywords": "premier league,match report,chelsea",
            "context": "positive"
        }
    }
}
```

#### Pre-Crawled Classification

For publishers who don't declare categories, the platform crawls and classifies pages:

```
Crawler (CronJob, similar to ads.txt crawler):
    1. Fetch publisher's sitemap or crawl pages
    2. Extract text content (title, headings, body text)
    3. Classify using ML model:
        Input: "Chelsea beat Arsenal 2-1 in Premier League thriller"
        Output: {IAB17-1: 0.95, IAB17-12: 0.3, IAB17: 0.99}
    4. Store classification in Postgres + L1 cache:
        url_pattern: "sportsmedia.com/football/*" -> [IAB17-1]
    5. SSP looks up classification when generating bid request
```

ML model trained in Python (`python/contextual/`), exported as ONNX, inference in Go for hot-path speed.

#### Keyword Targeting

Advertisers target specific keywords, not just categories:

```yaml
targeting:
  include:
    keywords: ["electric vehicles", "EV charging", "tesla"]
  exclude:
    keywords: ["accident", "recall", "lawsuit"]
```

The DSP matches advertiser keywords against the page keywords in the bid request. Matching is case-insensitive with stemming (e.g. "charging" matches "charger").

Implemented in `pkg/targeting/contextual.go`. Classification cache in `pkg/targeting/classification.go`. Crawler in `cmd/crawler/` (or extends `cmd/adstxt/`).

### 20. Sequential Messaging

Show a sequence of creatives to the same user in a defined order over time. Premium feature for brand storytelling.

#### How It Works

```
Advertiser configures a sequence on a line item:
    Step 1: "Meet Acme Shoes" (awareness creative)     -> show first
    Step 2: "How they're made" (consideration creative) -> show after user saw step 1
    Step 3: "20% off this week" (conversion creative)   -> show after user saw steps 1 and 2
    |
    v
User visits publisher site, bid request arrives:
    |
    v
DSP checks sequence state for this user:
    Redis: seq:{user_id}:{line_item_id} -> current_step = 0 (hasn't seen any)
    |
    v
    Step 0: serve creative for Step 1 ("Meet Acme Shoes")
    After impression confirmed: Redis SET seq:{user_id}:{line_item_id} = 1
    |
    v
Next day, same user visits another site:
    Redis: seq:{user_id}:{line_item_id} -> current_step = 1
    Serve creative for Step 2 ("How they're made")
    After impression: SET step = 2
    |
    v
Next visit:
    Step = 2 -> serve Step 3 ("20% off this week")
    After impression: SET step = 3 (complete)
    |
    v
Sequence complete: stop serving this line item to this user (or loop if configured)
```

#### Sequence Configuration

```json
{
    "line_item_id": "li_123",
    "creative_rotation": "sequential",
    "sequence": [
        {"step": 1, "creative_id": "cr_awareness", "min_gap_hours": 0},
        {"step": 2, "creative_id": "cr_consideration", "min_gap_hours": 24},
        {"step": 3, "creative_id": "cr_conversion", "min_gap_hours": 12}
    ],
    "on_completion": "stop",
    "sequence_timeout_days": 14
}
```

| Setting | What it controls |
|---|---|
| `min_gap_hours` | Minimum time between steps (don't show step 2 immediately after step 1) |
| `on_completion` | `stop` (don't serve again) or `loop` (restart from step 1) |
| `sequence_timeout_days` | If user doesn't see all steps within 14 days, reset and start over |

#### Redis State

```
Key: seq:{user_id}:{line_item_id}
Value: {step: 2, last_step_at: 1716825600}
TTL: 14 days (sequence_timeout_days)
```

Lightweight - one Redis key per user per sequential line item. The Ad Server reads this before selecting a creative.

#### Reporting

| Metric | What it shows |
|---|---|
| Step completion rate | % of users who reached each step (funnel) |
| Step drop-off | Where users stop progressing |
| Sequence completion rate | % of users who saw all steps |
| Average sequence duration | How long from step 1 to final step |
| Conversion by step reached | Do users who see all 3 steps convert more than those who see only 1? |

Implemented in `pkg/adserving/sequence.go`. Redis state checked during creative selection in the Ad Server.

### 21. Dynamic Creative Optimisation (DCO)

Auto-generate creative variants from a template at serve time. Instead of uploading 50 banner variations, upload a template and components. The Ad Server assembles the best combination per impression.

#### How DCO Works

```
Advertiser creates a DCO template:
    ┌──────────────────────────────┐
    │  [HEADLINE]                  │
    │  [PRODUCT_IMAGE]             │
    │  [DESCRIPTION]               │
    │  [CTA_BUTTON]   [PRICE]      │
    │  [BACKGROUND_COLOR]          │
    └──────────────────────────────┘

With component variants:
    HEADLINE:     ["Best Running Shoes", "Run Faster Today", "New Season Collection"]
    PRODUCT_IMAGE: [shoes_red.jpg, shoes_blue.jpg, shoes_white.jpg]
    DESCRIPTION:   ["Lightweight comfort", "Built for speed", "All-terrain grip"]
    CTA_BUTTON:    ["Shop Now", "Learn More", "Get 20% Off"]
    PRICE:         ["$89.99", "From $79.99"]
    BACKGROUND:    ["#FFFFFF", "#F0F8FF", "#1A1A2E"]

Total combinations: 3 * 3 * 3 * 3 * 2 * 3 = 486 unique creatives
```

#### Serve-Time Assembly

```
Bid request arrives, line item with DCO wins auction
    |
    v
Ad Server selects components using multi-arm bandit:
    - HEADLINE: "Run Faster Today" (best CTR for this user's segment)
    - PRODUCT_IMAGE: shoes_blue.jpg (best for mobile users)
    - DESCRIPTION: "Built for speed" (best for sports segment)
    - CTA_BUTTON: "Get 20% Off" (best for evening traffic)
    - PRICE: "$89.99"
    - BACKGROUND: "#F0F8FF" (best for this publisher's page style)
    |
    v
Ad Server renders template with selected components:
    HTML output: fully assembled creative
    |
    v
Track which combination was served (for bandit learning):
    dco_variant: {headline: 2, image: 2, desc: 2, cta: 3, price: 1, bg: 2}
```

#### Bandit at Component Level

The existing multi-arm bandit (`pkg/adserving/bandit.go`) operates on whole creatives. DCO extends it to component-level:

```
Each component slot has its own bandit:
    HEADLINE bandit: variant 1 (CTR 1.1%), variant 2 (CTR 1.4%), variant 3 (CTR 0.9%)
    IMAGE bandit: variant 1 (CTR 1.0%), variant 2 (CTR 1.3%), variant 3 (CTR 1.1%)
    ...

Components are selected independently:
    Best headline + best image + best CTA = optimal combination

Or: correlated selection (some combinations work better together):
    "Run Faster Today" + shoes_blue.jpg + "Built for speed" = cohesive message
    Requires correlation tracking between component choices
```

For MVP: independent selection per component. Correlated selection as an optimisation later.

#### Signal-Driven Personalisation

Components can be selected based on signals, not just performance:

| Signal | Component adjustment |
|---|---|
| User segment = "existing_customer" | CTA = "Welcome Back" instead of "Learn More" |
| Weather = hot | IMAGE = cold_drink.jpg instead of hot_coffee.jpg |
| Time = evening | BACKGROUND = dark theme |
| Device = mobile | Shorter headline (max 20 chars) |
| Geo = DE | Language = German variant |

Implemented in `pkg/adserving/dco.go`. Templates stored in Minio/S3 as HTML with `{{SLOT}}` placeholders. Component variants stored alongside. Bandit state in Redis per component per line item.

### 22. Inventory Quality Scoring

Beyond fraud detection (binary: is it fraud?), quality scoring rates **how valuable** a placement is for bid decisions.

#### Quality Score Components

| Signal | Weight | Source | What it measures |
|---|---|---|---|
| Viewability rate (30-day) | 25% | Analytics rollups | Are ads on this placement actually seen? |
| Click-through rate (30-day) | 15% | Analytics rollups | Do users engage with ads here? |
| Fraud rate (30-day) | 20% | Fraud scoring data | How much invalid traffic? |
| Brand safety incidents | 10% | Fraud/quality logs | How often are ads blocked for content issues? |
| Fill rate | 10% | Auction logs | Is there demand for this placement? |
| Page load speed | 10% | Publisher data / crawler | Slow pages = lower ad viewability |
| Publisher trust tier | 10% | Manual + historical | How long has this publisher been active? Track record? |

#### Score Calculation

```
Placement X quality score:
    viewability_rate = 0.72 (72%)     -> normalised: 0.72 * 0.25 = 0.180
    ctr = 0.008 (0.8%)               -> normalised: 0.80 * 0.15 = 0.120  (relative to category avg)
    fraud_rate = 0.02 (2%)           -> normalised: 0.98 * 0.20 = 0.196  (inverted: lower = better)
    brand_safety_incidents = 0       -> normalised: 1.00 * 0.10 = 0.100
    fill_rate = 0.85 (85%)          -> normalised: 0.85 * 0.10 = 0.085
    page_speed = fast                -> normalised: 0.90 * 0.10 = 0.090
    publisher_trust = established    -> normalised: 0.80 * 0.10 = 0.080

    Quality score = 0.851 (out of 1.0)
```

#### Quality Score as a Bid Modifier

Quality score feeds into the existing bid modifier system:

```yaml
bid_modifiers:
  quality_score:
    "0.8-1.0": +15%     # premium inventory, bid higher
    "0.6-0.8": +0%      # average, base bid
    "0.4-0.6": -20%     # below average, bid lower
    "0.0-0.4": -50%     # poor quality, minimal bid (or exclude)
```

Or as a continuous modifier: `modifier = 0.5 + (quality_score * 0.5)`. Score 1.0 = full bid. Score 0.0 = half bid.

#### Score Computation Pipeline

Scores computed by `cmd/optimise/` CronJob (daily), stored in Postgres, cached in Redis L1 for hot-path access during bid evaluation.

Implemented in `pkg/targeting/quality.go`. Scores per placement. Updated daily by optimisation pipeline.

### 23. Reach/Frequency Forecasting and Campaign Planning

Advertiser asks: "If I target UK mobile users aged 25-34 with $50K budget, how many people will I reach?"

#### Forecasting Engine

```
POST /v1/api/planning/forecast
Body: {
    targeting: {geo: ["UK"], device: ["mobile"], segments: ["age_25_34"]},
    budget: 50000,
    duration_days: 30,
    bid_strategy: "cpm",
    base_bid: 3.00
}
    |
    v
Forecasting engine:
    1. Query historical data:
        - How many impressions matched this targeting in the last 30 days?
        - What was the average clearing price for this targeting?
        - How many unique users were in this audience (HLL)?
    |
    v
    2. Project:
        available_impressions = historical_daily_avg * 30 = 2,000,000
        affordable_impressions = budget / avg_clearing_price = 50000 / 2.80 = 17,857,000
        actual_impressions = min(available, affordable) = 2,000,000
        estimated_reach = unique_users_in_audience * (1 - (1 - 1/unique_users)^impressions)
        estimated_frequency = impressions / reach
    |
    v
Response: {
    estimated_impressions: 2000000,
    estimated_reach: 450000,
    estimated_frequency: 4.4,
    estimated_spend: 50000,
    estimated_cpm: 2.80,
    confidence: "medium",
    recommendations: [
        "Budget exceeds available inventory. Consider broadening targeting.",
        "At current CPMs, you could reach this audience with $28K budget."
    ]
}
```

#### Budget Recommendation

```
POST /v1/api/planning/budget-recommendation
Body: {
    targeting: {geo: ["UK"], device: ["mobile"]},
    goal: {reach: 1000000, frequency: 3},
    duration_days: 30
}

Response: {
    recommended_budget: 84000,
    recommended_bid: 2.80,
    estimated_reach: 1000000,
    estimated_frequency: 3.0,
    confidence: "medium",
    alternatives: [
        {budget: 50000, reach: 600000, frequency: 2.5},
        {budget: 120000, reach: 1200000, frequency: 3.5}
    ]
}
```

#### Media Plan Comparison

```
POST /v1/api/planning/compare
Body: {
    plans: [
        {name: "Broad", targeting: {geo: ["UK"]}, budget: 50000, bid: 2.00},
        {name: "Narrow", targeting: {geo: ["UK_london"], segments: ["tech"]}, budget: 50000, bid: 4.00}
    ]
}

Response: {
    comparison: [
        {name: "Broad", reach: 800000, frequency: 2.1, cpm: 2.10, impressions: 1680000},
        {name: "Narrow", reach: 150000, frequency: 8.5, cpm: 3.90, impressions: 1275000}
    ],
    recommendation: "Broad plan reaches 5x more users. Narrow plan has higher frequency but audience is smaller."
}
```

Implemented in `pkg/reporting/forecast.go`. Uses historical data from analytics rollups + HLL audience size estimates.

### 24. Loss Notification Processing and Win-Rate Feedback Loop

The mechanism that makes bid shading actually work. Without it, the shading model has no data.

#### DSP Loss Notification Endpoint

The DSP exposes an endpoint that the Exchange calls for every losing bid:

```
Exchange determines auction result:
    Winner: DSP B at $3.00
    Losers: DSP A at $2.50 (outbid), DSP C at $1.80 (below floor)
    |
    v
Exchange calls each losing DSP's loss endpoint:
    GET /v1/openrtb/loss?bid_id=bid_A&reason=102&clearing_price=3.00
    GET /v1/openrtb/loss?bid_id=bid_C&reason=100&clearing_price=3.00
    |
    v
Exchange also sends win notice to winner:
    GET /v1/openrtb/win?bid_id=bid_B&price=3.00
```

#### DSP Loss Processing

```
Loss notification arrives at DSP:
    {bid_id: "bid_A", reason: 102, clearing_price: 3.00, placement_id: "pl_456"}
    |
    v
1. Record in win/loss log (Redis sorted set per placement):
    ZADD winloss:{pl_456} {timestamp} {type:"loss", our_bid:2.50, clearing:3.00, reason:102}
    |
    v
2. Update win-rate curve for this placement:
    At bid $2.50: we lost. Record data point.
    |
    v
3. Classify loss reason and take action:

    | Reason | Action |
    |--------|--------|
    | 100: Below floor | Increase min bid for this placement (floor is higher than we thought) |
    | 102: Outbid | Record clearing price. Adjust shading model: we need to bid higher here. |
    | 103: Publisher blocked | Stop bidding on this placement for this advertiser. |
    | 104: Creative not approved | Flag creative for review. Stop using it on this publisher. |
    | 2: Timeout | Investigate DSP latency. Not a bid price issue. |
```

#### Win-Rate Curve Construction

The shading model needs win-rate curves: "if I bid $X on placement Y, what's my probability of winning?"

```
Placement pl_456 win/loss data (last 7 days):
    Bid $1.00: 200 bids, 10 wins  -> win rate 5%
    Bid $1.50: 300 bids, 45 wins  -> win rate 15%
    Bid $2.00: 500 bids, 150 wins -> win rate 30%
    Bid $2.50: 400 bids, 200 wins -> win rate 50%
    Bid $3.00: 300 bids, 225 wins -> win rate 75%
    Bid $3.50: 200 bids, 180 wins -> win rate 90%
    Bid $4.00: 100 bids, 97 wins  -> win rate 97%
    |
    v
Fit a curve (logistic regression):
    win_rate(bid) = 1 / (1 + exp(-k * (bid - midpoint)))
    midpoint = $2.50 (50% win rate)
    k = 2.5 (steepness)
    |
    v
Shading uses this curve:
    "To win 60% of the time on this placement, bid $2.75"
    "Current target win rate is 50%, so bid $2.50"
```

#### Storage and Update Frequency

| Data | Storage | Update frequency |
|---|---|---|
| Individual win/loss events | Redis sorted sets per placement (7-day window) | Real-time (on every win/loss notification) |
| Win-rate curves | Redis hash per placement (fitted curve parameters) | Hourly (optimisation pipeline re-fits curves) |
| Aggregate win-rate stats | Analytics store (rollups) | Daily rollup |

#### Win-Rate Curve Granularity

Curves can be computed at multiple levels:

| Level | When to use |
|---|---|
| Per placement | Default - most accurate |
| Per placement + hour of day | If winning patterns differ by time |
| Per placement + device | If mobile vs desktop have different competition |
| Per publisher (all placements) | When a specific placement has insufficient data |
| Per geo + device (fallback) | For new placements with no history |

The shading model uses the most granular curve available. Falls back to coarser levels when data is insufficient (< 100 data points).

#### Metrics

| Metric | What it tracks |
|---|---|
| `dsp_win_rate{placement, geo}` | Current win rate per placement |
| `dsp_loss_reasons{reason}` | Distribution of loss reasons |
| `dsp_avg_clearing_price{placement}` | Average clearing price from win/loss data |
| `dsp_shading_efficiency` | (avg_bid - avg_clearing_price) / avg_clearing_price (how much we're saving) |

Implemented in `pkg/auction/feedback.go` for loss processing. `pkg/auction/winrate.go` for curve construction. `pkg/auction/shading.go` reads curves for bid calculation.

### Implementation (Features 17-24)

| Component | Location |
|---|---|
| Pacing algorithm | `pkg/pacing/` - PID controller, throttle calculation, feedback loop |
| Pacing modes | `pkg/pacing/modes.go` - even, ASAP, front-loaded, custom curve |
| Day boundary job | `cmd/reporting --mode=day-boundary` - daily resets, flight management |
| Timezone handling | `pkg/pacing/timezone.go` - timezone-aware day boundary calculation |
| Budget rollover | `pkg/pacing/rollover.go` - carry unspent budget forward |
| Contextual classification | `pkg/targeting/contextual.go` - publisher-declared + URL pattern + keyword matching |
| Content crawler | Extends `cmd/adstxt/` or new `cmd/crawler/` - pre-crawl and classify pages |
| ML classifier | `python/contextual/` - train content classification model, export to ONNX |
| Sequential messaging | `pkg/adserving/sequence.go` - sequence state tracking, step selection |
| DCO template engine | `pkg/adserving/dco.go` - template rendering, component selection, per-slot bandit |
| DCO templates | Minio/S3 - HTML templates with `{{SLOT}}` placeholders |
| Inventory quality scoring | `pkg/targeting/quality.go` - composite score calculation |
| Quality score pipeline | `cmd/optimise/` - daily score computation from analytics data |
| Reach/frequency forecasting | `pkg/reporting/forecast.go` - historical projection, budget recommendation |
| Planning API | Gateway: `/v1/api/planning/*` |
| Loss notification handler | DSP: `GET /v1/openrtb/loss` endpoint |
| Loss processing | `pkg/auction/feedback.go` - classify reason, update placement data |
| Win-rate curve builder | `pkg/auction/winrate.go` - logistic regression fit from win/loss data |
| Win-rate storage | Redis sorted sets (raw events) + Redis hash (fitted curves) |

---

## Video Ads, SSAI, and CTV

### Overview

Video advertising is the highest-value programmatic format. Display ads average $2-5 CPM. Video averages $10-20 CPM. CTV (Connected TV) commands $20-50 CPM. The platform needs to support video from the foundation up, not bolt it on later.

Three delivery methods:

```
Client-side (VAST/VPAID)          Server-side (SSAI)               CTV
─────────────────────            ─────────────────────           ─────────────────
Player requests ad               Ad stitched into stream         Server-side to TV devices
from ad server                   on the server                   Household targeting
Ad blocker CAN intercept         Ad blocker CANNOT detect        No cookies, no clicks
Web video, mobile apps           Live streaming, VOD             Smart TVs, Roku, Fire TV
$10-20 CPM                       $15-25 CPM                      $20-50 CPM
```

### VAST (Video Ad Serving Template)

VAST is the IAB standard XML format for video ad serving. When a video player needs to show an ad, it requests a VAST XML document that describes what to play and how to track it.

**VAST response from our Ad Server:**

```xml
<?xml version="1.0" encoding="UTF-8"?>
<VAST version="4.2">
  <Ad id="ad_123">
    <InLine>
      <AdSystem>AdTechMono</AdSystem>
      <AdTitle>Acme Shoes - 15s Pre-roll</AdTitle>
      <Impression><![CDATA[https://tracker.example.com/v1/t/imp?tid=${AUCTION_ID}&cid=${CAMPAIGN_ID}&type=video&sig=xyz]]></Impression>
      <Creatives>
        <Creative>
          <Linear>
            <Duration>00:00:15</Duration>
            <MediaFiles>
              <MediaFile delivery="progressive" type="video/mp4" width="1920" height="1080" bitrate="5000">
                <![CDATA[https://cdn.example.com/creatives/video_123_1080p.mp4]]>
              </MediaFile>
              <MediaFile delivery="progressive" type="video/mp4" width="1280" height="720" bitrate="2500">
                <![CDATA[https://cdn.example.com/creatives/video_123_720p.mp4]]>
              </MediaFile>
              <MediaFile delivery="progressive" type="video/mp4" width="640" height="360" bitrate="1000">
                <![CDATA[https://cdn.example.com/creatives/video_123_360p.mp4]]>
              </MediaFile>
            </MediaFiles>
            <TrackingEvents>
              <Tracking event="start"><![CDATA[https://tracker.example.com/v1/t/video?tid=${AUCTION_ID}&event=start]]></Tracking>
              <Tracking event="firstQuartile"><![CDATA[https://tracker.example.com/v1/t/video?tid=${AUCTION_ID}&event=q1]]></Tracking>
              <Tracking event="midpoint"><![CDATA[https://tracker.example.com/v1/t/video?tid=${AUCTION_ID}&event=mid]]></Tracking>
              <Tracking event="thirdQuartile"><![CDATA[https://tracker.example.com/v1/t/video?tid=${AUCTION_ID}&event=q3]]></Tracking>
              <Tracking event="complete"><![CDATA[https://tracker.example.com/v1/t/video?tid=${AUCTION_ID}&event=complete]]></Tracking>
              <Tracking event="skip"><![CDATA[https://tracker.example.com/v1/t/video?tid=${AUCTION_ID}&event=skip]]></Tracking>
              <Tracking event="mute"><![CDATA[https://tracker.example.com/v1/t/video?tid=${AUCTION_ID}&event=mute]]></Tracking>
              <Tracking event="pause"><![CDATA[https://tracker.example.com/v1/t/video?tid=${AUCTION_ID}&event=pause]]></Tracking>
            </TrackingEvents>
            <VideoClicks>
              <ClickThrough><![CDATA[https://tracker.example.com/v1/t/click?tid=${AUCTION_ID}&redir=https%3A%2F%2Facme.com]]></ClickThrough>
            </VideoClicks>
          </Linear>
        </Creative>
        <Creative>
          <CompanionAds>
            <Companion width="300" height="250">
              <StaticResource creativeType="image/png">
                <![CDATA[https://cdn.example.com/creatives/companion_300x250.png]]>
              </StaticResource>
              <CompanionClickThrough><![CDATA[https://acme.com/shoes]]></CompanionClickThrough>
            </Companion>
          </CompanionAds>
        </Creative>
      </Creatives>
    </InLine>
  </Ad>
</VAST>
```

**Video tracking events (richer than display):**

| Event | When it fires | Billing relevance |
|---|---|---|
| `impression` | Ad starts loading | Standard impression |
| `start` | Video starts playing | CPV (cost per view) billing trigger |
| `firstQuartile` | 25% watched | Engagement metric |
| `midpoint` | 50% watched | Engagement metric |
| `thirdQuartile` | 75% watched | Engagement metric |
| `complete` | 100% watched (or skippable threshold met) | CPCV (cost per completed view) billing trigger |
| `skip` | User clicked skip button | Engagement metric (not billed for CPCV) |
| `mute` | User muted audio | Quality signal |
| `pause` | User paused video | Engagement signal |
| `clickThrough` | User clicked the ad | CPC billing trigger |
| `viewableImpression` | IAB viewability met (50% pixels, 2s continuous for video) | vCPM billing trigger |

**New tracker endpoint for video events:**

```
GET /v1/t/video?tid={trace_id}&cid={campaign_id}&event={event_type}&sig={signature}
-> HTTP 204 No Content
```

### VMAP (Video Multiple Ad Playlist)

VMAP describes **when** ads should play within a video. The player requests a VMAP document, which contains ad break positions pointing to VAST responses.

```xml
<vmap:VMAP version="1.0">
  <!-- Pre-roll: ad before content -->
  <vmap:AdBreak timeOffset="start" breakType="linear">
    <vmap:AdSource>
      <vmap:AdTagURI><![CDATA[https://adserver.example.com/v1/vast?placement=preroll&...]]></vmap:AdTagURI>
    </vmap:AdSource>
  </vmap:AdBreak>

  <!-- Mid-roll: ad at 5 minutes into content -->
  <vmap:AdBreak timeOffset="00:05:00" breakType="linear">
    <vmap:AdSource>
      <vmap:AdTagURI><![CDATA[https://adserver.example.com/v1/vast?placement=midroll&...]]></vmap:AdTagURI>
    </vmap:AdSource>
  </vmap:AdBreak>

  <!-- Post-roll: ad after content ends -->
  <vmap:AdBreak timeOffset="end" breakType="linear">
    <vmap:AdSource>
      <vmap:AdTagURI><![CDATA[https://adserver.example.com/v1/vast?placement=postroll&...]]></vmap:AdTagURI>
    </vmap:AdSource>
  </vmap:AdBreak>
</vmap:VMAP>
```

**Ad break positions:**

| Position | When | CPM range | User tolerance |
|---|---|---|---|
| Pre-roll | Before content starts | Highest ($15-25) | Medium (expected, like TV) |
| Mid-roll | During content | High ($12-20) | Low (interruptive, but captive) |
| Post-roll | After content ends | Lowest ($5-10) | Highest (user can leave) |
| Overlay | During content, non-interrupting | Low ($3-8) | High (doesn't block content) |

### Ad Pods (Multiple Ads per Break)

A single ad break can contain multiple ads in sequence (like a TV commercial break):

```
Ad Break at 5:00 (mid-roll, 60 seconds):
    Ad 1: Acme Shoes (15s) - $18 CPM
    Ad 2: TechCo Phones (30s) - $22 CPM
    Ad 3: FoodBrand Snacks (15s) - $15 CPM
```

**Ad pod rules:**
- Maximum pod duration (e.g. 60s, 90s, 120s)
- Maximum ads per pod (e.g. 3-5)
- Competitive separation within the pod (no two car brands in same break)
- Deduplicated advertisers (same advertiser shouldn't appear twice in one pod)
- Position preference: some advertisers pay premium for first or last position in pod

The Exchange runs a **pod auction** - multiple winners selected to fill the pod duration, with competitive separation enforced:

```
Pod auction for 60s mid-roll break:
    |
    v
Collect bids from all DSPs (each bid includes ad duration)
    |
    v
Select combination that maximises revenue while:
    - Total duration <= 60s
    - No competitive conflicts
    - No duplicate advertisers
    - Respects position preferences
    |
    v
Result: [Ad1 15s, Ad2 30s, Ad3 15s] = 60s, $55 total CPM
```

Implemented in `pkg/auction/pods.go` - a bin-packing problem with constraints.

### Server-Side Ad Insertion (SSAI / Instream)

The key differentiator. Instead of the player requesting ads client-side (which ad blockers intercept), the ad is **stitched into the video stream on the server**. The viewer sees one continuous stream.

**Client-side (blockable):**
```
Video player                 Ad server
    |                            |
    +-- "give me an ad" -------> |  <-- ad blocker intercepts this request
    |                            |
    +-- <blocked>                |
    |                            |
    User sees: no ad (revenue lost)
```

**Server-side / SSAI (unblockable):**
```
Video player                 Stitching server              Ad server
    |                            |                            |
    +-- "give me the stream" --> |                            |
    |                            +-- "need ad for break" ---> |
    |                            |                            |
    |                            | <-- VAST response -------- |
    |                            |                            |
    |                            | [stitches ad video into    |
    |                            |  content stream seamlessly]|
    |                            |                            |
    | <-- continuous stream ---- |                            |
    |     (content + ad look     |                            |
    |      identical to player)  |                            |
    |                            |
    User sees: ad plays seamlessly (cannot be blocked)
```

**SSAI architecture in our platform:**

```
Content provider uploads video to Minio/S3
    |
    v
Manifest manipulation server (cmd/ssai/):
    1. Receives HLS/DASH manifest request from player
    2. Parses manifest, identifies ad break positions
    3. For each break: calls Exchange for auction (same gRPC as normal)
    4. Receives winning VAST, transcodes ad creative to match stream specs
    5. Replaces content segments with ad segments in the manifest
    6. Returns modified manifest to player
    |
    v
Player streams modified manifest:
    - Content segments from content CDN
    - Ad segments from ad CDN
    - Player sees one continuous stream
    |
    v
Tracking: SSAI server fires impression/quartile/complete beacons
    server-side (not client-side, since player doesn't know about ads)
```

**Key SSAI components:**

| Component | What it does |
|---|---|
| **Manifest manipulator** | Rewrites HLS (.m3u8) / DASH (.mpd) manifests to insert ad segment URLs |
| **Ad transcoder** | Converts ad creative video to match content specs (resolution, bitrate, codec, segment duration) |
| **Session manager** | Tracks viewer session: which ads they've seen, frequency caps, competitive separation across breaks |
| **Beacon server** | Fires tracking beacons server-side on behalf of the player (since player can't fire them) |

**HLS manifest manipulation example:**

```
Original content manifest:
#EXTM3U
#EXT-X-TARGETDURATION:6
#EXTINF:6.0,
content_segment_001.ts
#EXTINF:6.0,
content_segment_002.ts
#EXT-X-CUE-OUT:DURATION=30    <-- ad break marker
#EXTINF:6.0,
content_segment_003.ts         <-- content during ad break (replaced)
...
#EXT-X-CUE-IN                  <-- end of ad break
#EXTINF:6.0,
content_segment_008.ts

Modified manifest (SSAI):
#EXTM3U
#EXT-X-TARGETDURATION:6
#EXTINF:6.0,
content_segment_001.ts
#EXTINF:6.0,
content_segment_002.ts
#EXTINF:6.0,
ad_segment_001.ts              <-- ad video segment (stitched in)
#EXTINF:6.0,
ad_segment_002.ts
#EXTINF:6.0,
ad_segment_003.ts
#EXTINF:6.0,
ad_segment_004.ts
#EXTINF:6.0,
ad_segment_005.ts              <-- 30s of ad (5 x 6s segments)
#EXTINF:6.0,
content_segment_008.ts
```

The player just requests segments in order. It can't tell which are content and which are ads.

### CTV (Connected TV)

CTV is video advertising on TV screens - Smart TVs, Roku, Fire TV, Apple TV, gaming consoles, set-top boxes. It's the bridge between traditional TV buying and programmatic.

**How CTV differs:**

| Concern | Web/Mobile | CTV |
|---|---|---|
| Device | Phone, tablet, laptop | TV screen in living room |
| User identity | Cookie, device ID, hashed email | Household IP, device graph, ACR (automatic content recognition) |
| Targeting | User-level | Household-level (multiple people watch one TV) |
| Interaction | Click, tap, scroll | No clicks (it's a TV). QR codes, remote-triggered actions. |
| Ad format | Display, native, video | Video only (15s, 30s, 60s) |
| Delivery | Client-side or SSAI | Almost always SSAI |
| Measurement | Impressions, clicks, conversions | Impressions, completion rate, reach, tune-away rate |
| CPMs | $2-20 | $20-50 (premium, TV-like reach) |
| Ad blocking | Common on web | Virtually impossible with SSAI |
| Content | Short-form, articles, apps | Long-form (movies, shows, live sports) |

**CTV bid request signals:**

```json
{
    "device": {
        "devicetype": 3,           // 3 = Connected TV
        "make": "Roku",
        "model": "Roku Ultra",
        "os": "Roku OS",
        "osv": "12.5",
        "ua": "Roku/DVP-12.5 (12.5.0)",
        "ip": "203.0.113.42",
        "ifa": "roku-device-id-123",
        "connectiontype": 1        // Ethernet (typical for TV)
    },
    "app": {
        "bundle": "com.pluto.tv",
        "name": "Pluto TV",
        "cat": ["IAB1-7"],         // Television
        "storeurl": "https://channelstore.roku.com/details/..."
    },
    "imp": [{
        "video": {
            "mimes": ["video/mp4"],
            "protocols": [2, 3],    // VAST 2.0, VAST 3.0
            "w": 1920,
            "h": 1080,
            "minduration": 15,
            "maxduration": 30,
            "linearity": 1,         // Linear (in-stream)
            "placement": 1          // In-stream
        }
    }]
}
```

**CTV-specific targeting:**

| Dimension | What it targets | Example |
|---|---|---|
| Household | IP-based household graph | "Target households that visited our website" |
| DMA / zip code | Geographic area | "Target the Greater London area" |
| Content genre | What they're watching | "Target users watching sports content" |
| Daypart | Time of day on TV schedule | "Prime time 8pm-11pm" |
| Device make/model | Specific TV platforms | "Target Roku devices only" |
| Network/channel | Content provider | "Target ads on Pluto TV" |

**CTV measurement (no clicks):**

Since users can't click a TV, measurement is different:

| Metric | How it's measured |
|---|---|
| Impressions | SSAI beacon fires server-side |
| Completion rate | % of viewers who watched to 100% (high on CTV - captive audience) |
| Reach | Household-level unique reach (not user-level) |
| Frequency | Impressions per household |
| Tune-away | Viewer changed channel during ad (measured via ACR or player heartbeat) |
| Brand lift | Survey-based (post-exposure survey to measure awareness/intent change) |
| Website visit lift | Did the household visit the advertiser's website after seeing the CTV ad? (matched via household IP) |
| Foot traffic | Did someone from the household visit a physical store? (matched via mobile device in same household) |

**Cross-screen attribution:**

CTV's biggest value: connecting TV exposure to digital actions:

```
Household sees CTV ad for Acme Shoes at 8:15pm
    |
    v
Identity graph: household IP 203.0.113.42
    -> Mobile device in same household: user_id "mob_456"
    -> Desktop in same household: user_id "desk_789"
    |
    v
8:30pm: mob_456 visits acme.com on phone
9:00pm: desk_789 purchases shoes on desktop
    |
    v
Attribution: CTV impression -> mobile site visit -> desktop conversion
    (cross-screen, household-level)
```

### Video Ad Billing Models

| Model | What advertiser pays for | Use case |
|---|---|---|
| CPM | Per 1000 impressions (ad started) | Brand awareness |
| CPCV | Per completed view (watched to end) | Engagement campaigns |
| CPV | Per view (varies: 2s, 5s, or 50%) | Awareness + some engagement |
| vCPM | Per 1000 viewable impressions (IAB video: 50% pixels, 2s) | Viewability-focused |
| CPM + completion bonus | Base CPM + bonus for high completion rate | Premium buys |

### Video Creative Requirements

| Spec | Requirement |
|---|---|
| Formats | MP4 (H.264), WebM (VP9) |
| Resolutions | 1920x1080, 1280x720, 640x360 (multiple for adaptive) |
| Durations | 6s, 15s, 30s, 60s (must match placement requirement) |
| File size | Max 50MB per resolution |
| Audio | Required (unlike display). Codec: AAC. |
| Companion banner | Optional 300x250 or 728x90 display ad shown alongside video |
| Skip button | Configurable: non-skippable, skippable after 5s, skippable immediately |

Video creatives are stored in Minio/S3 at multiple resolutions. The Ad Server (or SSAI transcoder) selects the appropriate resolution for the player's capabilities.

### Video Services Architecture

Video requires dedicated services because of the unique compute requirements (transcoding is CPU/GPU intensive), long-lived state (viewing sessions last 30min-2hr), and real-time constraints (live streaming ad breaks happen in seconds).

#### Service Map

```
Content provider                    Our platform                           Viewer

  Video stream ──> [Content CDN]
                        |
                        v
                   [SSAI Stitcher] <──gRPC──> [Exchange] <──> [DSPs]
                        |                         |
                        |                    [Ad Server]
                        |                         |
                   [Transcoder]              [Creative CDN]
                        |                         |
                        v                         v
                   Modified manifest ─────────────────────────> [Player]
                   (content + ads stitched)
                        |
                   [Session Manager] ──> [Tracker] ──> [NATS] ──> [Reporting]
                   (per-viewer state)     (beacons)
```

#### Service 1: SSAI Stitcher (`cmd/ssai/`)

The core video ad serving service. Rewrites HLS/DASH manifests to insert ad segments in place of content.

**Responsibilities:**
- Receive manifest requests from video players
- Parse content manifests and identify ad break markers (`EXT-X-CUE-OUT`/`EXT-X-CUE-IN` for HLS, `Period` markers for DASH)
- For each ad break: call Exchange via gRPC for pod auction
- Replace content segments with transcoded ad segments in the manifest
- Return modified manifest to player
- Maintain per-session state via Session Manager

**Two modes:**

| Mode | When | How it works |
|---|---|---|
| **VOD (Video on Demand)** | Movies, shows, catch-up | Ad decisions can be made ahead of time. Manifest pre-computed and cached. |
| **Live** | Sports, news, events | Ad breaks signaled in real-time by content provider. Auction + stitching must complete in <3 seconds. |

**VOD flow:**

```
Viewer starts watching a movie
    |
    v
Player requests manifest: GET /ssai/manifest/content_123.m3u8?session=sess_abc
    |
    v
SSAI Stitcher:
    1. Check session cache: already computed? -> serve cached manifest
    2. Not cached:
        a. Fetch original manifest from content CDN
        b. Parse: find 4 ad breaks (pre-roll, 2 mid-rolls, post-roll)
        c. For each break: call Exchange (pod auction)
            Pre-roll: 30s -> auction returns [Ad A 15s, Ad B 15s]
            Mid-roll 1: 60s -> auction returns [Ad C 30s, Ad D 15s, Ad E 15s]
            Mid-roll 2: 60s -> auction returns [Ad F 30s, Ad G 30s]
            Post-roll: 15s -> auction returns [Ad H 15s]
        d. Build modified manifest with ad segments
        e. Cache manifest for this session (viewer may seek/reload)
    3. Return modified manifest
    |
    v
Player streams segments in order (can't tell ads from content)
```

**Live flow:**

```
Live football match streaming
    |
    v
Content encoder signals: SCTE-35 marker "ad break in 3 seconds, duration 120s"
    |
    v
SSAI Stitcher receives signal via webhook or manifest polling:
    1. IMMEDIATELY call Exchange for pod auction (must complete in <500ms)
    2. Pod auction: fill 120s with ads
       -> Exchange runs parallel DSP calls (100ms timeout)
       -> Winners: [Ad A 30s, Ad B 30s, Ad C 30s, Ad D 30s] = 120s
    3. Verify transcoded segments exist for all winners (pre-transcoded on upload)
    4. Build ad break segment list
    5. Next manifest request from any viewer: includes ad segments for this break
    |
    v
Millions of viewers see the same ads at the same time (like TV)
    |
    v
But: ads CAN be personalised per viewer/household if using per-session manifests
    Viewer A in UK sees: UK-targeted ads
    Viewer B in US sees: US-targeted ads
    (different manifests per session, same content, different ads)
```

**Personalised vs shared ad breaks:**

| Approach | How | Scalability | Use case |
|---|---|---|---|
| **Shared** | Same ads for all viewers, one manifest | Simple, scales infinitely via CDN | Simulcast, low-value inventory |
| **Personalised** | Different ads per viewer, manifest per session | More compute (one auction per viewer per break) | Premium inventory, CTV |

For live events with millions of viewers, personalised breaks mean millions of concurrent auctions at break time. This is where horizontal scaling of the Exchange and SSAI Stitcher matters.

**HLS manifest manipulation detail:**

```
Original manifest with ad break markers:

#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:1

# Content before break
#EXTINF:6.000,
https://content-cdn.com/seg_001.ts
#EXTINF:6.000,
https://content-cdn.com/seg_002.ts

# Ad break marker (SCTE-35)
#EXT-X-CUE-OUT:30
#EXT-X-CUE-OUT-CONT:ElapsedTime=0,Duration=30
#EXTINF:6.000,
https://content-cdn.com/seg_003.ts    <-- replaced
#EXTINF:6.000,
https://content-cdn.com/seg_004.ts    <-- replaced
#EXTINF:6.000,
https://content-cdn.com/seg_005.ts    <-- replaced
#EXTINF:6.000,
https://content-cdn.com/seg_006.ts    <-- replaced
#EXTINF:6.000,
https://content-cdn.com/seg_007.ts    <-- replaced
#EXT-X-CUE-IN

# Content after break
#EXTINF:6.000,
https://content-cdn.com/seg_008.ts


Modified manifest (per session):

#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:1

# Content before break
#EXTINF:6.000,
https://content-cdn.com/seg_001.ts
#EXTINF:6.000,
https://content-cdn.com/seg_002.ts

# Ad break (stitched)
#EXTINF:6.000,
https://ad-cdn.com/sess_abc/ad_A_seg1.ts?beacon=start
#EXTINF:6.000,
https://ad-cdn.com/sess_abc/ad_A_seg2.ts
#EXTINF:3.000,
https://ad-cdn.com/sess_abc/ad_A_seg3.ts?beacon=complete
#EXTINF:6.000,
https://ad-cdn.com/sess_abc/ad_B_seg1.ts?beacon=start
#EXTINF:6.000,
https://ad-cdn.com/sess_abc/ad_B_seg2.ts
#EXTINF:3.000,
https://ad-cdn.com/sess_abc/ad_B_seg3.ts?beacon=complete

# Content after break
#EXTINF:6.000,
https://content-cdn.com/seg_008.ts
```

The ad segment URLs include session IDs and beacon triggers. When the player requests an ad segment, the SSAI service can fire tracking beacons server-side.

#### Service 2: Video Transcoder (`cmd/transcoder/`)

Converts uploaded ad creatives into the exact format needed for stream stitching. This is **not** real-time - it runs when creatives are uploaded.

**Why a separate service:**
- Transcoding is CPU/GPU intensive (minutes per creative)
- Different scaling profile - needs beefy machines, not many instances
- Must produce every resolution/bitrate/codec variant the content might need
- Can pre-process - don't wait until serve time

**Transcoding pipeline:**

```
Advertiser uploads video creative: "shoes_ad_30s.mp4" (1080p, H.264, 5Mbps)
    |
    v
Creative upload triggers transcoding job:
    |
    v
Transcoder produces variant matrix:

Resolution  | Codec  | Bitrate | Segment duration | Output
1920x1080   | H.264  | 5000k   | 6s               | shoes_ad_30s_1080p_h264_5000k/
1920x1080   | H.264  | 3000k   | 6s               | shoes_ad_30s_1080p_h264_3000k/
1280x720    | H.264  | 2500k   | 6s               | shoes_ad_30s_720p_h264_2500k/
1280x720    | H.264  | 1500k   | 6s               | shoes_ad_30s_720p_h264_1500k/
640x360     | H.264  | 1000k   | 6s               | shoes_ad_30s_360p_h264_1000k/
640x360     | H.264  | 500k    | 6s               | shoes_ad_30s_360p_h264_500k/
1920x1080   | HEVC   | 3000k   | 6s               | shoes_ad_30s_1080p_hevc_3000k/ (CTV)
    |
    v
Each variant is segmented into 6-second .ts files + .m3u8 sub-manifest:
    shoes_ad_30s_1080p_h264_5000k/
        manifest.m3u8
        seg_001.ts (0-6s)
        seg_002.ts (6-12s)
        seg_003.ts (12-18s)
        seg_004.ts (18-24s)
        seg_005.ts (24-30s)
    |
    v
All variants uploaded to Minio/S3
    |
    v
Creative record updated: transcoding_status = "complete", variants = [...]
```

**Segment duration matching:**

The ad segments MUST match the content's segment duration. If content uses 6s segments, ad must use 6s segments. If content uses 4s, ad must use 4s. The transcoder produces the most common durations (4s, 6s, 10s) by default. Custom durations on request.

**Transcoding as a K8s Job:**

Each creative upload spawns a K8s Job:
- Resource-intensive (request 2 CPU, 4GB RAM per transcode job)
- Parallelised across the variant matrix (each resolution/bitrate can transcode independently)
- Completion time: ~30s for a 15s ad, ~2min for a 60s ad
- Status tracked in Postgres, creative can't be used in auctions until transcoding completes

```
POST /v1/api/creatives (video upload)
    |
    v
Gateway -> Ad Server: store original in Minio/S3
    |
    v
Ad Server publishes NATS: adtech.video.transcode_requested
    |
    v
Transcoder consumes, spawns K8s Job per creative
    |
    v
Job completes, publishes: adtech.video.transcode_completed
    |
    v
Ad Server updates creative status: ready for auction
```

#### Service 3: Session Manager (`pkg/ssai/session.go`)

Tracks viewer state across an entire viewing session. A session starts when a viewer begins watching and ends when they stop.

**What it tracks per session:**

```json
{
    "session_id": "sess_abc",
    "viewer": {
        "user_id": "uuid-123",
        "household_id": "hh_456",
        "device_type": "ctv",
        "ip": "203.0.113.42",
        "geo": "UK_london"
    },
    "content": {
        "content_id": "movie_789",
        "genre": "action",
        "duration_seconds": 7200,
        "publisher_id": "pub_123"
    },
    "ad_breaks_seen": [
        {
            "break_index": 0,
            "position": "pre-roll",
            "ads": [
                {"creative_id": "cr_A", "advertiser_id": "adv_1", "category": "automotive"},
                {"creative_id": "cr_B", "advertiser_id": "adv_2", "category": "beverages"}
            ]
        }
    ],
    "frequency_caps_session": {
        "adv_1": 1,
        "adv_2": 1,
        "category:automotive": 1,
        "category:beverages": 1
    },
    "started_at": "2026-05-27T20:00:00Z",
    "last_heartbeat": "2026-05-27T20:35:00Z"
}
```

**Why session state matters:**

| Concern | Without session manager | With session manager |
|---|---|---|
| Competitive separation across breaks | Same advertiser could win every break | Track which advertisers/categories appeared, enforce cross-break separation |
| Frequency cap within session | User sees same ad in every break | Track ads seen, enforce "max 2 times per session" |
| Ad pod diversity | Same ad could repeat | Ensure creative diversity across the session |
| Viewer drop-off tracking | Don't know when viewer left | Heartbeat tracking, know exactly when each viewer stopped watching |
| Mid-session targeting update | Same targeting for every break | Adjust targeting based on which ads the viewer has already seen |

**Session storage:**

Sessions are stored in Redis with TTL:
- Key: `ssai:session:{session_id}`
- TTL: 4 hours (covers a long movie + buffer)
- Eviction: session explicitly closed or TTL expires

Redis is used (not Postgres) because:
- Sessions are short-lived (hours, not days)
- High read/write frequency (every manifest request reads session, every ad break updates it)
- If Redis loses a session, the viewer just gets a fresh session (minor UX impact, not data loss)

#### SSAI Beacon Firing (Server-Side Tracking)

In SSAI, the video player doesn't know about ads - it just plays segments. So tracking beacons must be fired **server-side** by the SSAI service, not client-side.

**How it works:**

```
Player requests ad segment: GET /ssai/segment/sess_abc/ad_A_seg3.ts
    |
    v
SSAI serves the segment AND fires beacons:
    |
    v
Is this the first segment of the ad? -> fire "start" beacon
Is this the 25% mark? -> fire "firstQuartile" beacon
Is this the 50% mark? -> fire "midpoint" beacon
Is this the 75% mark? -> fire "thirdQuartile" beacon
Is this the last segment? -> fire "complete" beacon
    |
    v
Beacon = publish to NATS: adtech.events.video
    {trace_id, session_id, creative_id, event_type: "midpoint", ...}
```

**Segment-to-quartile mapping:**

```
30-second ad with 6-second segments (5 segments):
    seg_001 (0-6s):   "start" beacon
    seg_002 (6-12s):  "firstQuartile" beacon (25% = 7.5s, closest segment boundary)
    seg_003 (12-18s): "midpoint" beacon (50% = 15s)
    seg_004 (18-24s): "thirdQuartile" beacon (75% = 22.5s)
    seg_005 (24-30s): "complete" beacon
```

**What if the viewer stops watching mid-ad?**

If the player stops requesting segments (viewer left, changed channel, closed app):
- No more segment requests -> no more beacons fired
- Session manager detects missing heartbeat after 30 seconds
- Records the last segment played -> accurate quartile tracking
- Ad billed appropriately: start was fired, complete was not -> not billed for CPCV

#### Live Streaming: SCTE-35 Ad Break Signaling

Live content providers signal ad breaks using SCTE-35 markers in the transport stream. Our SSAI Stitcher needs to detect and respond to these.

**SCTE-35 integration:**

```
Content encoder (at the live event venue)
    |
    v
Inserts SCTE-35 splice_insert command into transport stream:
    "Ad break starting NOW, duration 120 seconds"
    |
    v
Our SSAI Stitcher detects the marker via:
    Option A: Manifest conditioner (watches manifest updates, detects EXT-X-CUE-OUT)
    Option B: SCTE-35 webhook (content provider calls our API directly)
    Option C: Direct transport stream parsing (most complex, most real-time)
    |
    v
Break detected -> pod auction -> manifest modified -> ads served
```

For MVP, **Option A** (manifest conditioner) is simplest - we poll/watch the live manifest for `EXT-X-CUE-OUT` markers and react.

**Timing constraints for live:**

| Step | Time budget | Notes |
|---|---|---|
| Detect SCTE-35 marker | 0ms | As soon as manifest updates |
| Run pod auction (Exchange) | 100-500ms | Parallel DSP calls with timeout |
| Verify ad segments available | 10ms | Pre-transcoded, just check existence |
| Build modified manifest | 10ms | String manipulation |
| Serve to next player request | 0ms (passive) | Player polls manifest every segment duration |
| **Total** | **< 1 second** | Must complete before next manifest poll |

#### Content Provider Integration

Publishers with video content connect via:

| Integration method | How | Use case |
|---|---|---|
| **Manifest URL** | Publisher provides their HLS/DASH manifest URL. Our SSAI proxies it and injects ads. | VOD, simple integration |
| **SCTE-35 webhook** | Publisher calls our API when ad breaks are available. | Live streaming |
| **Direct content ingest** | Publisher uploads content to our platform. We host and serve with ads. | Full-service publishers |

**Publisher configuration for video:**

```json
{
    "publisher_id": "pub_123",
    "video_config": {
        "content_manifest_url": "https://content-cdn.publisher.com/live/master.m3u8",
        "ad_break_detection": "manifest_marker",
        "default_break_duration_seconds": 60,
        "max_ads_per_break": 4,
        "min_ad_duration_seconds": 15,
        "personalization": "per_session",
        "session_timeout_minutes": 240,
        "companion_banner_enabled": true,
        "companion_sizes": ["300x250", "728x90"]
    }
}
```

### OpenRTB Video Object

Added to bid requests for video inventory:

```json
{
    "imp": [{
        "video": {
            "mimes": ["video/mp4", "video/webm"],
            "protocols": [2, 3, 5, 6],
            "w": 1920,
            "h": 1080,
            "minduration": 5,
            "maxduration": 30,
            "linearity": 1,
            "placement": 1,
            "startdelay": 0,
            "skip": 1,
            "skipafter": 5,
            "api": [1, 2],
            "companiontype": [1, 2],
            "ext": {
                "content_genre": "sports",
                "break_position": "midroll",
                "break_index": 2,
                "session_ads_seen": 3,
                "is_live": true
            }
        }
    }]
}
```

The `ext` fields give DSPs context about the viewing session so they can bid smarter (e.g. bid higher for first break when viewer is most engaged).

### NATS Subjects for Video

| Subject | Publisher | Subscribers | Payload |
|---|---|---|---|
| `adtech.events.video` | SSAI beacon server / Tracker | Reporting (analytics + billing) | VideoEvent{trace_id, session_id, event_type, quartile} |
| `adtech.video.transcode_requested` | Ad Server | Transcoder | TranscodeRequest{creative_id, variants_needed} |
| `adtech.video.transcode_completed` | Transcoder | Ad Server | TranscodeComplete{creative_id, variants_produced} |
| `adtech.video.session_started` | SSAI | Reporting | SessionStartEvent{session_id, viewer, content} |
| `adtech.video.session_ended` | SSAI | Reporting | SessionEndEvent{session_id, duration, breaks_seen, ads_watched} |

### Implementation

| Component | Location |
|---|---|
| **SSAI Stitcher service** | `cmd/ssai/` - manifest manipulation, segment proxying, break detection |
| **Video Transcoder service** | `cmd/transcoder/` - creative variant generation, K8s Job spawning |
| **Session Manager** | `pkg/ssai/session.go` - per-viewer session state in Redis |
| **Beacon server** | `pkg/ssai/beacons.go` - server-side tracking, segment-to-quartile mapping |
| **SCTE-35 detection** | `pkg/ssai/scte35.go` - manifest marker parsing, webhook handler |
| **Manifest manipulator** | `pkg/ssai/manifest.go` - HLS/DASH parsing and rewriting |
| **VAST XML generation** | `pkg/adserving/vast.go` - for client-side video (non-SSAI) |
| **VMAP generation** | `pkg/adserving/vmap.go` - ad break schedule |
| **Ad pod auction** | `pkg/auction/pods.go` - multi-winner bin-packing |
| **Video creative validation** | `pkg/adserving/video.go` - duration, resolution, codec checks |
| **CTV bid handling** | `pkg/openrtb/ctv.go` - CTV device, app, video objects |
| **Household targeting** | `pkg/targeting/household.go` - IP-based household matching |
| **Cross-screen attribution** | `pkg/billing/attribution.go` - household-to-device matching |
| **Video creative storage** | Minio/S3 - original + all transcoded variants |

### K8s Deployment

| Service | Scaling | Resources |
|---|---|---|
| SSAI Stitcher | HPA based on manifest requests/sec | Low CPU, moderate memory (manifest manipulation is string work) |
| Transcoder | Job-based (one K8s Job per creative upload) | High CPU (2-4 cores per job), 4GB RAM, optional GPU |
| Session state | Redis (same cluster, dedicated keyspace `ssai:session:*`) | Depends on concurrent viewers |

### Video Integration with Existing Systems

Video is not a standalone feature - it touches every part of the platform. Each existing system needs video-specific handling.

#### Creative Review for Video

Video creatives go through additional validation beyond display:

| Check | Display | Video |
|---|---|---|
| Dimensions | Image size matches declared format | Resolution matches, aspect ratio correct |
| File size | Max 2MB | Max 50MB per resolution variant |
| Duration | N/A | Must be 6s, 15s, 30s, or 60s exactly |
| Audio | N/A | Must have audio track, loudness within CALM/R128 limits |
| Codec | N/A | H.264 Baseline/Main/High profile, AAC audio |
| Frame rate | N/A | Must match target content frame rates (24/25/30fps) |
| Content scan | Image classification | Frame sampling for content policy (every 1s) |
| Transcoding | N/A | Must successfully transcode to all variants before approval |

Creative review status for video: `uploaded -> transcoding -> transcoded -> auto-scan -> manual review (if flagged) -> approved/rejected`. Cannot be used in auctions until fully transcoded AND approved.

#### Deals for Video

Video deals are the highest-value deals on the platform. Additional deal configuration for video:

```json
{
    "deal_id": "deal_555",
    "deal_type": "pg",
    "format": "video",
    "video_config": {
        "break_positions": ["pre-roll"],
        "pod_position": "first",
        "min_duration": 15,
        "max_duration": 30,
        "skip_policy": "non-skippable",
        "companion_required": true,
        "guaranteed_completions": 500000
    }
}
```

PG deals for video may guarantee completions (not just impressions): "500,000 completed views at $25 CPM."

#### Video-Specific Targeting Dimensions

Added to the targeting engine alongside existing display dimensions:

| Dimension | Include example | Exclude example |
|---|---|---|
| `format` | video | display (video-only line item) |
| `break_position` | pre-roll, mid-roll | post-roll (avoid low-engagement position) |
| `content_genre` | sports, entertainment | news (brand safety for some advertisers) |
| `live_vs_vod` | live | vod (live events premium) |
| `ad_duration` | 15s, 30s | 60s (shorter attention span on mobile) |
| `pod_position` | first-in-pod | last-in-pod (first position premium) |
| `device_type` | ctv | mobile (CTV-only buy) |

#### Video-Specific Bid Modifiers

```yaml
bid_modifiers:
  break_position:
    pre-roll: +30%
    mid-roll: +0%
    post-roll: -40%
  pod_position:
    first: +20%
    last: -10%
  live_vs_vod:
    live: +25%
    vod: +0%
  content_genre:
    sports: +15%
    entertainment: +0%
```

#### Video Frequency Capping

**Status.** The core cap — per-user + per-household + advertiser-configured
per-campaign limit/window — IS enforced for video (and native/audio), same as
display, via `cmd/adserver/freqcap.go`. Non-display formats reach it through the
SSP's cap-only ad-server call (`ServeRequest.Channel` set → the ad server runs
the cap and returns allowed/429 without rendering; see the "Frequency Capping"
section above). The additional video-specific dimensions in the table below are
**PLANNED, not built** — deliberately deferred (see "not needed now" rationale:
nothing in the sim/demosite would exercise them yet).

Additional cap dimensions for video beyond the standard display caps (PLANNED):

| Dimension | Example | Redis key |
|---|---|---|
| Per user per session | "Max 2 video ads per viewing session" | `fc:{user}:{session}:video` |
| Per user per content | "Max 1 ad per break in this show" | `fc:{user}:{content_id}:break` |
| Per advertiser per hour (video) | "Max 3 video ads from Acme per hour" | `fc:{user}:{advertiser}:video:h` |
| Per pod position | "Don't show this creative first-in-pod more than once per session" | `fc:{user}:{creative}:first_pod:{session}` |

**What building this would take** (if/when a use case appears):
1. A **session id** and **content id** on the ad request — the platform models
   neither today. The SSP would derive/accept a `session` (e.g. per player load)
   and the publisher would pass `content_id` (the show/asset); both ride through
   to `ServeRequest`.
2. Extend `cmd/adserver/freqcap.go` with the extra keyed counters above (the
   `fc:` prefix here would join the existing `adserver:freqcap:` scheme).
3. Pod-position caps additionally need the pod-slot index (the pod builder in
   `pkg/ssai/pods.go` already has ordering) threaded to the cap check.
4. Advertiser-facing controls to set these dimensions (today only limit/window
   per campaign is configurable).

Recommendation: only the per-session cap is likely worth it, and only for a
deliberate CTV demo — not as gap-fill.

#### Video Billing Integration

| Billing model | Event source | When billable |
|---|---|---|
| CPM (video) | `adtech.events.video` with event_type=`start` | Ad started playing |
| CPCV | `adtech.events.video` with event_type=`complete` | Ad played to 100% (or skippable threshold) |
| CPV | `adtech.events.video` with event_type=`midpoint` or time threshold | Configurable: 2s, 5s, or 50% |
| vCPM (video) | `adtech.events.video` with event_type=`viewable` | 50% pixels visible for 2 continuous seconds (IAB video standard) |

Reserve-settle pattern for CPCV: budget reserved on `start`, settled on `complete`, released if viewer skips or drops off before completion.

#### Video Reporting Metrics

Added to the reporting service alongside display metrics:

| Metric | Calculation |
|---|---|
| Video completion rate (VCR) | complete events / start events |
| View-through rate (VTR) | midpoint events / start events |
| Average watch time | sum(duration_watched) / start events |
| Skip rate | skip events / start events |
| Quartile drop-off | chart: 100% at start -> X% at Q1 -> Y% at mid -> Z% at Q3 -> W% complete |
| Audio-on rate | (start - mute events) / start events |
| Companion CTR | companion clicks / companion impressions |
| CPCV (effective) | total video spend / completed views |

#### Video Fraud Detection

Video-specific fraud patterns added to `pkg/fraud/`:

| Fraud type | Detection | Action |
|---|---|---|
| Auto-play muted background | Video starts in muted, hidden tab/window | Flag as non-viewable, don't bill for vCPM |
| Pre-roll hijacking | VAST response intercepted and replaced with different creative | VAST signature verification |
| SSAI impression inflation | SSAI service reporting more impressions than actual viewers | Cross-reference session count with CDN request logs |
| Stacked video players | Multiple video players on same page, all "playing" | Viewability check, only one player visible |
| Bot completion fraud | Perfect 100% completion rate from suspicious IPs | Statistical analysis, no real viewer has 100% VCR |

#### Video Quality Controls for Publishers

Publishers can set video-specific quality rules:

```json
{
    "placement_id": "pl_video_preroll",
    "video_quality_controls": {
        "max_ad_duration_seconds": 30,
        "max_ads_per_break": 3,
        "max_ads_per_hour": 12,
        "skip_policy": "skippable_after_5s",
        "blocked_ad_categories": ["IAB25"],
        "require_companion": false,
        "max_pod_duration_seconds": 90,
        "bumper_creative_id": "cr_bumper_123"
    }
}
```

#### Video in Publisher Simulator

The publisher simulator adds a video page template:

```
┌──────────────────────────────────────────┐
│  Video Simulator                          │
│                                           │
│  ┌─────────────────────────────────┐     │
│  │                                 │     │
│  │      [Video Player]             │     │
│  │                                 │     │
│  │   ▶ 0:00 / 15:00              │     │
│  │                                 │     │
│  │   Pre-roll playing: Acme (15s) │     │
│  │   trace: abc-123               │     │
│  │                                 │     │
│  └─────────────────────────────────┘     │
│                                           │
│  ┌─ 300x250 Companion ─┐                │
│  │  [Companion banner]  │                │
│  └──────────────────────┘                │
│                                           │
│  Debug: VAST requested ✓ | Start ✓ |    │
│  Q1 ✓ | Mid ⏳ | Q3 ○ | Complete ○     │
└──────────────────────────────────────────┘
```

Uses HLS.js for client-side playback with VAST integration. Shows quartile progression in real time.

#### Video in Optimisation Pipelines

Video-specific optimisations in `cmd/optimise/`:

| Pipeline | Video optimisation |
|---|---|
| Bid optimisation | Separate shading models for video vs display (different price ranges) |
| Creative performance | 15s vs 30s duration performance comparison. Skip rate analysis. |
| Pod position | Which position in pod drives best completion rate? |
| Break position | Pre-roll vs mid-roll performance for this advertiser |
| Publisher yield | Optimal number of ads per break to maximise revenue without viewer drop-off |

### CDN Strategy for Ad Segments

During live events with personalised breaks, millions of segment requests hit simultaneously. Without CDN, the Minio/S3 origin melts.

**Architecture:**

```
Transcoder produces ad segments -> uploads to Minio/S3 (origin)
    |
    v
CDN (CloudFront / Fastly / Cloudflare) caches segments at edge
    |
    v
SSAI Stitcher rewrites manifest URLs to point to CDN:
    Original: https://minio.internal/ad_segments/cr_123/1080p/seg_001.ts
    Rewritten: https://ad-cdn.example.com/cr_123/1080p/seg_001.ts?token=abc&exp=1716825600
```

| Concern | Approach |
|---|---|
| CDN selection | Configurable via live config. CloudFront for prod, Minio directly for local. |
| Signed URLs | Token-authenticated URLs with expiry to prevent hotlinking |
| Cache invalidation | On creative re-transcode or pull: CDN invalidation API call |
| Origin shield | CDN tiered caching to reduce origin load |
| Multi-CDN | Failover to secondary CDN if primary has issues |
| Local dev | No CDN - SSAI serves segments directly from Minio. Same code, different URL prefix via Kustomize. |

### Audio Loudness Normalisation

Regulatory requirement. The transcoder must normalise audio levels.

| Standard | Region | Target loudness | Applies to |
|---|---|---|---|
| ATSC A/85 (CALM Act) | US | -24 LUFS | All TV/CTV ads |
| EBU R128 | EU | -23 LUFS | All broadcast ads |
| ARIB TR-B32 | Japan | -24 LUFS | Japanese broadcast |

**Transcoding pipeline addition:**

```
Original creative uploaded
    |
    v
Step 1: Measure loudness (LUFS meter)
    Result: -18 LUFS (too loud by 6 LUFS)
    |
    v
Step 2: Apply gain adjustment
    Target: -24 LUFS for US market
    Adjustment: -6 LUFS
    |
    v
Step 3: True peak limiting
    Ensure no sample exceeds -1 dBTP (true peak limit)
    |
    v
Step 4: Store normalised audio alongside original
    US variant: -24 LUFS
    EU variant: -23 LUFS
    |
    v
SSAI selects correct loudness variant based on viewer's geo
```

If a creative exceeds loudness limits and cannot be auto-normalised (e.g. extreme distortion), it's **flagged for manual review** rather than auto-rejected. The advertiser is notified.

### SSAI Failover and Slate Content

Live breaks cannot show dead air. Failover strategy:

```
SSAI needs to fill a 60-second break:
    |
    v
1. Run pod auction -> Exchange returns winners
    |
    +-- Full fill (60s of ads) -> serve ads ✓
    |
    +-- Partial fill (45s of ads, 15s unfilled):
    |       Options (configurable per publisher):
    |       a. Serve 45s of ads + 15s of slate/filler
    |       b. Serve 45s of ads, return to content early
    |       c. Re-auction with lower floor prices for remaining 15s
    |
    +-- No fill (0 bids):
    |       Options:
    |       a. Serve publisher's house ads (self-promotional)
    |       b. Serve slate content (branded "we'll be right back")
    |       c. Return to content (no break)
    |
    +-- Auction timeout (Exchange didn't respond in time):
    |       -> Serve cached fallback ads (pre-selected, always transcoded and ready)
    |
    +-- Transcoded segments missing for winner:
            -> Skip that ad, serve next winner. If none ready -> slate.
```

**Slate content:**

Publishers configure slate/filler for their video placements:

```json
{
    "video_failover": {
        "slate_creative_id": "cr_slate_publisher_123",
        "house_ad_creative_ids": ["cr_house_1", "cr_house_2"],
        "short_fill_policy": "slate",
        "no_fill_policy": "house_ads",
        "timeout_policy": "cached_fallback",
        "cached_fallback_pool": ["cr_fallback_1", "cr_fallback_2", "cr_fallback_3"]
    }
}
```

Slate and house ad creatives are pre-transcoded to all variants during publisher onboarding. Always ready.

**Bumpers:**

Short branded transitions (2-3 seconds) between content and ads:

```
Content -> [Bumper: "Brought to you by..."] -> Ad 1 -> Ad 2 -> [Bumper: "Back to the show"] -> Content
```

Configured per publisher placement. Bumper duration is subtracted from total break duration before pod auction.

### ABR Multi-Rendition Manifest Rewriting

Real streams have multiple quality levels. The SSAI stitcher must rewrite ALL of them.

**Master playlist (unchanged):**

```
#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
https://ssai.example.com/session/sess_abc/1080p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720
https://ssai.example.com/session/sess_abc/720p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360
https://ssai.example.com/session/sess_abc/360p.m3u8
```

The SSAI stitcher must generate a modified variant playlist for **every rendition** in the master:

```
Session sess_abc, break 1, winning ad "cr_123":
    Rewrite 1080p.m3u8 -> insert cr_123 1080p segments
    Rewrite 720p.m3u8  -> insert cr_123 720p segments
    Rewrite 360p.m3u8  -> insert cr_123 360p segments
```

If the player switches bitrate mid-ad (e.g. network conditions change), the ad segment at the new bitrate must exist. This is why the transcoder produces every resolution variant.

**`EXT-X-DISCONTINUITY` tags:**

At ad boundaries (content -> ad, ad -> ad, ad -> content), the stream may change codec profile, sample rate, or timing. Players need `EXT-X-DISCONTINUITY` tags to handle this:

```
#EXTINF:6.000,
content_segment_002.ts
#EXT-X-DISCONTINUITY            <-- codec/timing may change
#EXTINF:6.000,
ad_A_seg_001.ts
...
#EXT-X-DISCONTINUITY            <-- back to content codec/timing
#EXTINF:6.000,
content_segment_008.ts
```

### Beacon Accuracy and Playback Verification

Server-side beacons fire when segments are **requested**, not when they are **displayed**. This overcounts.

**The problem:**

```
Player buffer: [seg_003] [seg_004] [seg_005] [ad_seg_001] [ad_seg_002]
                                               ^
                                    Player requested this (beacon fires)
                                    But viewer is still watching seg_003
                                    Viewer closes app before reaching ad_seg_001
                                    -> Impression counted but never seen
```

**Mitigation strategies:**

| Strategy | How | Accuracy |
|---|---|---|
| **Segment request = impression** (current) | Fire beacon on request. Simple but overcounts. | Low (~90%) |
| **Heartbeat verification** | Player sends periodic heartbeats. If heartbeat stops before segment plays, void the impression. | Medium (~95%) |
| **Client-side verification callback** | SSAI injects a small JS/SDK callback in companion content that confirms playback reached the ad. | High (~98%) |
| **Buffer-adjusted beacons** | Delay beacon firing by estimated buffer depth (e.g. if player buffers 12s ahead, delay beacon by 12s. If no more requests come, void it). | Medium (~95%) |

For MVP, use **heartbeat verification**. The session manager expects heartbeats every 10s. If a heartbeat gap exceeds 30s, mark all beacons fired during the gap as `unverified`. Billing excludes unverified beacons.

### DVR and Time-Shift

When a viewer watches a live stream on delay, ad decisions need special handling.

```
Live edge:     [content] [AD BREAK] [content] [content]
                          ^ live viewers see this break now

DVR viewer (15 min behind):
               [content] [content] [content]
                          ^ DVR viewer is here, break hasn't arrived yet
```

**Three approaches:**

| Approach | How | Trade-off |
|---|---|---|
| **Same ads** | DVR viewer sees the same ads from the live break. Manifest cached. | Simple. But advertiser's campaign may have ended/paused. |
| **Fresh auction** | New auction for DVR viewer at their playback position. | Best targeting but more compute. |
| **Hybrid** | Same ads if within 30min of live edge. Fresh auction if >30min behind. | Balanced. |

**Configuration per publisher:**

```json
{
    "dvr_config": {
        "mode": "hybrid",
        "fresh_auction_threshold_minutes": 30,
        "dvr_window_hours": 4,
        "expired_campaign_handling": "replace_with_fresh"
    }
}
```

The session manager tracks each viewer's playback position relative to the live edge and selects the appropriate ad decision strategy.

### Creative Conditioning (Color Space, Frame Rate, Codec Profile)

The transcoder must match content specifications beyond resolution and bitrate to avoid visible seams at stitch points.

| Parameter | What happens if mismatched | Transcoder action |
|---|---|---|
| **Color space** | HDR content (BT.2020) + SDR ad (BT.709) = brightness/colour jump | Convert ad to content's colour space. If content is HDR, tone-map ad up. |
| **Frame rate** | 25fps content + 30fps ad = judder at boundary | Re-encode ad to match content frame rate |
| **Codec profile** | H.264 High Profile content + Baseline Profile ad = decoder reset on some players | Re-encode ad to match content's profile/level |
| **Sample rate** | 48kHz content audio + 44.1kHz ad audio = audio glitch | Resample ad audio to match content |
| **Segment duration** | 6s content segments + 4s ad segments = playlist timing issues | Re-segment ad to match content segment duration |

**Content specification detection:**

When a publisher registers a video placement, the SSAI stitcher probes the content stream and extracts its specs:

```
Content stream analysis:
    Resolution: 1920x1080
    Codec: H.264 High Profile Level 4.1
    Frame rate: 25fps
    Color space: BT.709 (SDR)
    Audio: AAC 48kHz stereo
    Segment duration: 6.006s
    -> Store as placement video profile in Postgres
    -> Transcoder produces variants matching THIS profile
```

### Short Fill and Bumpers

**Short fill resolution:**

When the pod auction can't fill the full break duration, the SSAI stitcher applies the publisher's short fill policy:

| Policy | Behaviour | Use case |
|---|---|---|
| `slate` | Fill remaining time with slate content | Premium publishers (no dead air) |
| `early_return` | Return to content early | News, short-form content |
| `re_auction` | Re-auction remaining time with lower floors | Maximise fill rate |
| `stretch` | Distribute gap across ads as brief pauses | Not recommended (viewer notice) |
| `house_ads` | Fill with publisher's own promos | Publishers with house content |

**Bumper handling:**

```
Break duration: 90s
Bumper in: 3s ("Brought to you by Acme Platform")
Bumper out: 2s ("And now back to the show")
Available for ads: 90 - 3 - 2 = 85s
    -> Pod auction fills 85s
    -> SSAI stitcher: [bumper_in] [ad1] [ad2] [ad3] [bumper_out]
```

Bumpers are pre-transcoded publisher-configured creatives. Not auctioned - they're fixed content.

### Implementation Summary

| Component | Location |
|---|---|
| CDN URL rewriting | `pkg/ssai/cdn.go` - signed URL generation, CDN origin config |
| Audio normalisation | `cmd/transcoder/` - LUFS measurement + gain adjustment during transcode |
| Failover / slate | `pkg/ssai/failover.go` - slate selection, short fill policy, cached fallback pool |
| ABR manifest rewriting | `pkg/ssai/manifest.go` - multi-rendition rewriting, discontinuity tags |
| Beacon verification | `pkg/ssai/beacons.go` - heartbeat tracking, buffer-adjusted firing, unverified flagging |
| DVR handling | `pkg/ssai/dvr.go` - playback position tracking, fresh vs cached ad decisions |
| Creative conditioning | `cmd/transcoder/` - color space, frame rate, codec profile, audio sample rate matching |
| Content spec detection | `pkg/ssai/probe.go` - analyse content stream, extract video profile |
| Short fill / bumpers | `pkg/ssai/pods.go` - bumper insertion, short fill policy application |
| Video creative review | `pkg/adserving/video.go` - extended validation for duration, codec, loudness |
| Video targeting dimensions | `pkg/targeting/` - break_position, content_genre, live_vs_vod, pod_position |
| Video bid modifiers | `pkg/targeting/modifiers.go` - break position, live premium modifiers |
| Video frequency caps | Core per-user/household/campaign cap enforced (`cmd/adserver/freqcap.go`, all formats). Session-level / per-content / per-pod dimensions PLANNED — need a session id + content id on the request (not yet modelled). |
| Video fraud detection | `pkg/fraud/video.go` - auto-play muted, stacked players, bot completion |
| Video reporting metrics | `pkg/reporting/video.go` - VCR, VTR, skip rate, quartile drop-off |
| Video deal config | `pkg/deals/` - break position, pod position, skip policy, guaranteed completions |
| Video quality controls | `pkg/adserving/video.go` - max duration, max per hour, bumper config |

### For MVP

Video is phased:

1. **Phase 1 (foundation):** OpenRTB `video` object, video creative upload + validation (including loudness check), VAST XML from Ad Server, video tracking in Tracker
2. **Phase 2 (client-side):** Publisher simulator with HLS.js video player, VAST/VMAP playback, pre/mid/post-roll, video debug overlay
3. **Phase 3 (transcoder):** `cmd/transcoder/` service, variant matrix generation including audio normalisation, content spec matching
4. **Phase 4 (SSAI core):** `cmd/ssai/` stitcher service, multi-rendition manifest manipulation, session manager, server-side beacons with heartbeat verification
5. **Phase 5 (SSAI production):** CDN integration, failover/slate, bumpers, short fill, ABR boundary handling, discontinuity tags
6. **Phase 6 (live):** SCTE-35 detection, real-time pod auctions, DVR/time-shift handling, personalised live ad breaks
7. **Phase 7 (CTV):** CTV bid requests, household targeting, cross-screen attribution, CTV-specific fraud detection

---

## Audio Ads (Radio and Podcasts)

### Overview

Audio advertising covers streaming radio, podcasts, and music services. It reuses most of the video SSAI infrastructure but with simpler creatives (no video track) and different measurement (listen-through instead of viewability).

**Three delivery contexts:**

| Context | How ads are delivered | Ad break signaling | CPMs |
|---|---|---|---|
| **Streaming radio** | Live SSAI - audio ads stitched into live stream | SCTE-35 / manifest markers (same as live video) | $10-20 |
| **Podcast (dynamic insertion)** | Ads inserted into episodes at marked positions on download/stream | RSS feed manipulation + manifest markers | $25-50 |
| **Music streaming** | Ads between songs or during breaks | Player SDK triggers ad request between tracks | $15-25 |

### What We Reuse from Video

| Component | Reuse | What changes |
|---|---|---|
| SSAI Stitcher (`cmd/ssai/`) | Yes | Audio-only HLS playlists, no video segments |
| Transcoder (`cmd/transcoder/`) | Yes, simpler | 2-3 bitrate variants, no resolution/frame rate. Loudness normalisation already built. |
| Session Manager | Yes | Typically shorter sessions (podcast episode = 30-60min) |
| Beacon Server | Yes | Same quartile tracking + listen-through rate |
| Pod Auction | Yes | Same bin-packing, audio durations (15s, 30s, 60s) |
| Exchange | Yes | New `imp.audio` object in bid request |
| Tracker | Yes | `GET /v1/t/audio?event={type}` endpoint |
| CDN | Yes | Same signed URL pattern, much smaller files |
| Failover/slate | Yes | Audio slate ("we'll be right back" jingle) |

### What's New for Audio

#### DAAST (Digital Audio Ad Serving Template)

DAAST is the IAB standard for audio ad serving - structurally almost identical to VAST but for audio:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<DAAST version="1.0">
  <Ad id="audio_ad_123">
    <InLine>
      <AdSystem>AdTechMono</AdSystem>
      <AdTitle>Acme Shoes - 30s Audio Spot</AdTitle>
      <Impression><![CDATA[https://tracker.example.com/v1/t/audio?tid=${AUCTION_ID}&event=impression]]></Impression>
      <Creatives>
        <Creative>
          <Linear>
            <Duration>00:00:30</Duration>
            <MediaFiles>
              <MediaFile type="audio/mpeg" bitrate="128">
                <![CDATA[https://ad-cdn.example.com/audio/acme_shoes_30s_128k.mp3]]>
              </MediaFile>
              <MediaFile type="audio/mpeg" bitrate="64">
                <![CDATA[https://ad-cdn.example.com/audio/acme_shoes_30s_64k.mp3]]>
              </MediaFile>
              <MediaFile type="audio/aac" bitrate="128">
                <![CDATA[https://ad-cdn.example.com/audio/acme_shoes_30s_128k.aac]]>
              </MediaFile>
            </MediaFiles>
            <TrackingEvents>
              <Tracking event="start"><![CDATA[https://tracker.example.com/v1/t/audio?tid=${AUCTION_ID}&event=start]]></Tracking>
              <Tracking event="firstQuartile"><![CDATA[https://tracker.example.com/v1/t/audio?tid=${AUCTION_ID}&event=q1]]></Tracking>
              <Tracking event="midpoint"><![CDATA[https://tracker.example.com/v1/t/audio?tid=${AUCTION_ID}&event=mid]]></Tracking>
              <Tracking event="thirdQuartile"><![CDATA[https://tracker.example.com/v1/t/audio?tid=${AUCTION_ID}&event=q3]]></Tracking>
              <Tracking event="complete"><![CDATA[https://tracker.example.com/v1/t/audio?tid=${AUCTION_ID}&event=complete]]></Tracking>
              <Tracking event="skip"><![CDATA[https://tracker.example.com/v1/t/audio?tid=${AUCTION_ID}&event=skip]]></Tracking>
            </TrackingEvents>
          </Linear>
        </Creative>
        <Creative>
          <CompanionAds>
            <Companion width="300" height="250">
              <StaticResource creativeType="image/png">
                <![CDATA[https://ad-cdn.example.com/companion/acme_shoes_300x250.png]]>
              </StaticResource>
              <CompanionClickThrough><![CDATA[https://acme.com/shoes]]></CompanionClickThrough>
            </Companion>
          </CompanionAds>
        </Creative>
      </Creatives>
    </InLine>
  </Ad>
</DAAST>
```

#### Podcast Dynamic Ad Insertion

Podcasts have a unique delivery model: episodes are pre-recorded, but ads can be inserted dynamically at download or stream time. This means a listener downloading an episode today gets different ads than someone downloading the same episode next week.

**How it works:**

```
Podcast publisher creates episode:
    - Records content with ad markers: "ad break here, 60 seconds"
    - Uploads episode audio to our platform (or provides RSS feed URL)
    |
    v
Listener requests episode:
    GET /audio/podcast/episode_123.m3u8?listener=user_abc
    |
    v
SSAI Stitcher:
    1. Fetch original episode audio
    2. Find ad markers (pre-roll, mid-roll x2, post-roll)
    3. Run auction for each break (personalised per listener)
    4. Stitch ad audio into episode
    5. Return personalised audio stream
    |
    v
Listener hears:
    [Pre-roll ad: 30s] -> [Episode content: 10min] -> [Mid-roll ad: 60s] -> [Content: 15min] -> [Post-roll ad: 30s]
```

**Podcast RSS feed manipulation:**

For podcast apps that use RSS (Apple Podcasts, Overcast, etc.), we modify the RSS feed to point episode audio URLs through our SSAI:

```xml
<!-- Original RSS -->
<enclosure url="https://publisher.com/episodes/ep123.mp3" type="audio/mpeg"/>

<!-- Modified RSS (our proxy) -->
<enclosure url="https://ssai.adtech.com/podcast/ep123.mp3?feed=pub_123&sig=xyz" type="audio/mpeg"/>
```

When the podcast app downloads the episode, it hits our SSAI, which stitches in personalised ads.

#### Audio-Specific Measurement

| Metric | What it measures | How |
|---|---|---|
| **Listen-through rate (LTR)** | % of listeners who heard the full ad | complete events / start events |
| **Audibility** | Was audio actually playing (not muted)? | Player SDK reports mute state. SSAI can't verify without SDK. |
| **Background listening** | Was the app in foreground or background? | Player SDK reports app state. Background = lower engagement but still listening. |
| **Download vs stream** | Did listener download episode (offline) or stream? | Download = beacons fire on next online session. Stream = real-time beacons. |
| **Completion rate by position** | Pre-roll vs mid-roll vs post-roll LTR | Pre-roll: ~90% (captive), mid-roll: ~85% (engaged), post-roll: ~40% (leaving) |

**Offline/download challenge:**

Podcast listeners often download episodes for offline listening. Tracking beacons can't fire without internet. Solutions:

| Approach | How | Trade-off |
|---|---|---|
| **Client-side queuing** | Player SDK queues beacons, fires when back online | Accurate but requires SDK integration |
| **Pessimistic counting** | Count download as impression, assume average LTR for completion | Simple but less accurate |
| **Streaming only** | Only serve dynamic ads to streaming listeners, not downloads | Misses offline audience |

For MVP, use **pessimistic counting** for downloads and real-time beacons for streaming.

#### Audio Creative Requirements

| Spec | Requirement |
|---|---|
| Formats | MP3 (most compatible), AAC (better quality), OGG (open source) |
| Bitrates | 128kbps (standard), 64kbps (mobile/low bandwidth) |
| Durations | 15s, 30s, 60s |
| Sample rate | 44.1kHz or 48kHz |
| Channels | Stereo preferred, mono acceptable |
| Loudness | -16 LUFS (podcast standard, louder than broadcast) or -24 LUFS (broadcast/radio) |
| File size | Max 5MB per bitrate variant |
| Companion banner | Optional 300x250 display ad for apps that support it |

Audio transcoding is much simpler than video - fewer variants, no resolution/frame rate concerns, just bitrate and format.

#### OpenRTB Audio Object

```json
{
    "imp": [{
        "audio": {
            "mimes": ["audio/mpeg", "audio/mp4"],
            "minduration": 15,
            "maxduration": 60,
            "protocols": [1, 2, 3],
            "startdelay": 0,
            "feed": 2,
            "stitched": 1,
            "ext": {
                "podcast_name": "The Sports Hour",
                "podcast_genre": "sports",
                "episode_number": 145,
                "is_live": false
            }
        }
    }]
}
```

| Field | Meaning |
|---|---|
| `feed` | 1=music, 2=podcast, 3=radio |
| `stitched` | 1=SSAI (server-side), 0=client-side |
| `startdelay` | 0=pre-roll, >0=mid-roll (seconds into content), -1=generic mid, -2=generic post |

#### Audio-Specific Targeting

| Dimension | Example |
|---|---|
| `audio_feed_type` | podcast, radio, music |
| `podcast_genre` | sports, news, comedy, true_crime, business |
| `podcast_name` | specific show targeting |
| `listening_context` | commute (morning/evening), workout, background |
| `device_type` | smart_speaker, car, headphones, phone_speaker |

Smart speaker targeting (Alexa, Google Home) is unique - no visual companion possible, voice CTA only ("say 'Alexa, buy Acme shoes'").

#### Audio Billing Models

| Model | What advertiser pays for | Use case |
|---|---|---|
| CPM (audio) | Per 1000 ad starts | Brand awareness |
| CPCL (cost per completed listen) | Per completed listen (100% heard) | Engagement / podcast |
| CPL (cost per listen) | Per listen threshold (e.g. 50% heard) | Awareness with quality |

Reserve-settle pattern for CPCL: same as video CPCV.

#### Audio Integration with Existing Systems

| System | Audio integration |
|---|---|
| Creative review | Audio-specific validation: duration, codec, loudness (podcast -16 LUFS vs radio -24 LUFS) |
| Deals | Audio PG deals common in podcasts: "50,000 completed listens on The Sports Hour at $40 CPM" |
| Frequency capping | Per-listener per-podcast: "max 1 audio ad per episode from this advertiser" |
| Fraud | Audio-specific: bot listeners, muted playback detection, download farming |
| Reporting | LTR, completion rate by position, download vs stream ratio, companion CTR |
| Publisher simulator | Audio player template with waveform visualisation and quartile debug |
| Quality controls | Max ad duration, max ads per episode, genre restrictions |

#### NATS Subjects for Audio

| Subject | Publisher | Subscribers | Payload |
|---|---|---|---|
| `adtech.events.audio` | Tracker / SSAI beacon server | Reporting (analytics + billing) | AudioEvent{trace_id, event_type, listen_duration} |
| `adtech.audio.transcode_requested` | Ad Server | Transcoder | AudioTranscodeRequest{creative_id} |
| `adtech.audio.transcode_completed` | Transcoder | Ad Server | AudioTranscodeComplete{creative_id, variants} |

#### Implementation

| Component | Location |
|---|---|
| DAAST XML generation | `pkg/adserving/daast.go` - builds DAAST response with tracking |
| Audio tracker endpoint | Tracker: `GET /v1/t/audio?event={type}` |
| Podcast RSS manipulation | `pkg/ssai/podcast.go` - RSS feed rewriting, episode proxy |
| Audio creative validation | `pkg/adserving/audio.go` - duration, codec, loudness checks |
| Audio transcoding | `cmd/transcoder/` - audio variant generation (reuses video transcoder with audio-only mode) |
| Audio targeting | `pkg/targeting/` - feed_type, podcast_genre, listening_context |
| OpenRTB audio object | `pkg/openrtb/audio.go` |

### For MVP

Audio follows the same phased approach as video but is simpler:

1. **Phase 1:** OpenRTB `audio` object, audio creative upload + loudness validation, DAAST generation, audio tracking endpoint
2. **Phase 2:** Podcast dynamic insertion via SSAI (reuse existing stitcher with audio-only mode), RSS feed manipulation
3. **Phase 3:** Streaming radio support (live SSAI for audio), companion banner ads
4. **Phase 4:** Smart speaker targeting, offline/download beacon queuing

---

## Digital Out-of-Home (DOOH)

### Overview

Digital billboards, transit shelters, airport screens, mall displays, gas station pumps, elevator screens. Physical screens in public spaces, sold programmatically. No cookies, no user-level targeting - it's about location, time, and audience estimation.

**How DOOH differs from other channels:**

| Concern | Display/Video/Audio | DOOH |
|---|---|---|
| Viewer | One person, one screen | Many people, one screen |
| Identity | Cookie, device ID, hashed email | No individual identity - aggregate audience |
| Targeting | User-level (segments, retargeting) | Location, time of day, venue type, weather, audience demographics |
| Impression | One ad shown to one person | One ad shown to many people (multiplied by audience estimate) |
| Measurement | Pixel fires, beacons | Camera-based audience detection, mobile device proximity, foot traffic |
| Interaction | Click, tap | QR code scan, NFC tap, mobile search lift |
| Creative | Image, HTML, video, audio | Static image, video loop (no audio in most locations), HTML5 |
| Duration | Instantaneous (display) or 15-60s (video) | Screen time slot: 10-15 seconds per rotation |
| Buying | Per impression (CPM) | Per play (cost per play) or per impression (multiplied by audience) |
| Ad blocker | Possible on web | Impossible (it's a physical screen) |

### How DOOH Fits Our Platform

```
Screen operator (publisher) registers screens
    |
    v
SSP generates bid requests with location + time + audience signals
    |
    v
Exchange runs auction (same as display/video)
    |
    v
Winning creative pushed to screen's content management system
    |
    v
Screen plays the ad for its time slot
    |
    v
Impression counted: plays * estimated audience = billable impressions
    |
    v
Tracking: proof-of-play confirmation from screen + audience measurement
```

### OpenRTB DOOH Bid Request

DOOH uses OpenRTB with a `dooh` object instead of `site`/`app`:

```json
{
    "dooh": {
        "id": "screen_123",
        "name": "Times Square Billboard #7",
        "venue_type": ["transit", "outdoor"],
        "venue_taxonomy": "1.1",
        "publisher": {"id": "pub_lamar", "name": "Lamar Advertising"}
    },
    "device": {
        "geo": {
            "lat": 40.7580,
            "lon": -73.9855,
            "type": 1
        }
    },
    "imp": [{
        "id": "1",
        "banner": {
            "w": 1920,
            "h": 1080,
            "mimes": ["image/jpeg", "image/png", "video/mp4"]
        },
        "ext": {
            "slot_duration_seconds": 10,
            "plays_per_hour": 360,
            "estimated_audience_per_play": 150,
            "screen_count": 1,
            "screen_type": "large_format",
            "weather": {"condition": "sunny", "temp_c": 22},
            "day_of_week": "friday",
            "time_of_day": "17:30"
        }
    }]
}
```

### DOOH-Specific Targeting

| Dimension | Example | Why |
|---|---|---|
| Venue type | transit, retail, airport, gym, gas_station, office_lobby | Different audiences at different venues |
| Geo (radius) | "Within 500m of our stores" | Drive foot traffic to nearby locations |
| DMA / postal code | "Greater London area" | Regional campaigns |
| Time of day | Morning rush 7-9am, lunch 12-2pm, evening rush 5-7pm | Commuter targeting |
| Day of week | Weekdays vs weekends | Different audience patterns |
| Weather | Sunny, rainy, cold, hot | "Show ice cream ads when it's hot" |
| Screen size | Large format (billboard), street level, indoor | Creative format matching |
| Audience demographics | Age/gender/income estimation from mobile data | Probabilistic audience composition |
| Points of interest | Near stadiums, shopping centres, universities | Contextual relevance |

### DOOH Measurement

No pixels, no clicks. Measurement is fundamentally different:

| Method | How it works | Accuracy |
|---|---|---|
| **Proof of play** | Screen confirms it displayed the creative (hardware log) | High - verifies the ad played |
| **Camera-based audience** | Camera on screen counts viewers, estimates attention (facing screen) | Medium - privacy concerns in some markets |
| **Mobile device proximity** | Count mobile devices in range via SDK data or telco data | Medium - sample-based extrapolation |
| **Foot traffic lift** | Did more people visit advertiser's store after seeing DOOH ad? | Low-medium - requires attribution model |
| **Mobile search lift** | Did mobile searches for the brand increase near the screen after ad played? | Medium - correlational |
| **QR code scans** | Direct response measurement | High but low volume (few people scan) |

**Impression calculation:**

```
Billable impressions = number_of_plays * estimated_audience_per_play * attention_factor

Example:
    Screen plays ad 6 times per hour
    Estimated 150 people see the screen per play (foot traffic data)
    Attention factor: 0.6 (estimated 60% actually look at the screen)

    Billable impressions per hour: 6 * 150 * 0.6 = 540 impressions
```

### DOOH Creative Requirements

| Spec | Requirement |
|---|---|
| Formats | JPEG, PNG (static), MP4 (video loop, no audio typically) |
| Resolutions | Match screen: 1920x1080, 3840x2160 (4K billboards), 1080x1920 (portrait screens) |
| Duration | Static: displayed for slot duration (10-15s). Video: exact slot duration, must loop cleanly. |
| File size | Max 50MB (screens may have limited connectivity) |
| Audio | Usually none (outdoor). Indoor screens may have audio. Specify per venue type. |
| Safe zones | Content must account for screen bezels, viewing distance (text size minimum) |

### DOOH Integration with Existing Systems

| System | DOOH integration |
|---|---|
| Exchange | Same auction engine. DOOH bids are typically lower frequency (screens refresh every 10-15s, not every page load). |
| DSP | Location-based targeting replaces user-based targeting. Bid modifiers for weather, time, venue type. |
| Billing | Cost per play or CPM (plays * audience). No CPC/CPA (no clicks). |
| Reporting | Plays, estimated impressions, audience reach, foot traffic lift. |
| Deals | PG deals common: "100 plays per day on Times Square screen for 30 days at $X." |
| Creative review | Screen-specific validation: resolution matches screen, safe zones, no audio for outdoor. |
| Fraud | Proof-of-play verification. Screen uptime monitoring. |

### Implementation

| Component | Location |
|---|---|
| OpenRTB DOOH object | `pkg/openrtb/dooh.go` - venue, screen, audience estimation fields |
| DOOH targeting | `pkg/targeting/dooh.go` - location radius, venue type, weather, time |
| Screen management | SSP handles screen registration as a special placement type |
| Proof-of-play tracking | Tracker: `POST /v1/t/dooh` - screen reports playback confirmation |
| Audience estimation | `pkg/dooh/audience.go` - mobile proximity data, foot traffic models |
| Creative validation | `pkg/adserving/dooh.go` - screen resolution matching, safe zones |

#### Built (MVP) — status 2026-08

The end-to-end money spine ships; the audience-estimation/creative-validation
detail above is still planned. What's live and e2e-proven
(`tests/e2e/dooh_test.go`, `TestDOOHAudienceMultiplier`):

| Concern | What ships | Where |
|---|---|---|
| Auction | `channel=dooh` routes to the **TimeSlot** strategy — one screen play = a single winner (first-price), not a pod. | `pkg/auction/timeslot.go` (delegates to SingleWinner), `SelectStrategy`; `cmd/exchange` `channelForRequest` reads `imp.ext.channel` |
| Serve | SSP `channel=dooh` builds a banner-shaped screen imp (`imp.ext.channel=dooh`) and short-circuits to return the winner. | `cmd/ssp/main.go` DOOH serve branch |
| Proof-of-play → audience multiplier | One signed beacon (`/v1/t/imp?ch=dooh&mult=N`) = **one** impression row carrying `impression_qty=N` (venue audience per play), and books the **full play cost** = per-impression cost × N. The row count stays 1 (one play); the audience is `SUM(impression_qty)`. | `cmd/tracker/main.go` (reads `mult` for `ch=dooh` only), `ImpressionEvent.ImpressionQty`, ClickHouse `impressions.impression_qty Int32 DEFAULT 1` |
| Money | `clearing_price_usd` on the row is the full play cost, so billing (books on impression) and prepay drawdown are lossless with no rollup change. | `ClearingPriceUSD = impCost × qty` |

**Count metric SHIPPED** (2026-08): the reporting QueryEngine's impression *count*
metric is now `SUM(impression_qty)` for the impressions table (`query.go`,
table-aware — equals `COUNT(*)` for every `qty=1` row, so non-DOOH channels are
unaffected), so a DOOH play reports as its audience. This also fixes the app-side
rollup (computed via `store.Query`); the ClickHouse rollup MVs
(`impressions_rollup_hourly/daily`) were rewired to `sum(impression_qty)` too (DROP
+ recreate; historical rollup rows are forward-only, not backfilled). Cost/eCPM were
already exact via `clearing_price_usd`.

### DOOH Cross-System Integration Detail

#### DOOH Creative Review

| Check | Requirement |
|---|---|
| Resolution | Must match target screen(s) exactly (1920x1080, 3840x2160, 1080x1920 portrait) |
| Safe zones | Text/logos within 90% of frame (viewing distance varies) |
| Contrast | Minimum contrast ratio for outdoor readability in sunlight |
| Video loop | Must loop seamlessly (no black frame at end) |
| Duration | Must exactly match slot duration (10s, 15s) |
| File size | Max 50MB (some screens have limited bandwidth) |
| Audio | Rejected for outdoor venues. Allowed for indoor with publisher approval. |
| Content policy | No adult content, no political ads in some venues, comply with local outdoor advertising laws |

#### DOOH Deals

```json
{
    "deal_id": "deal_dooh_789",
    "deal_type": "pg",
    "format": "dooh",
    "dooh_config": {
        "screens": ["screen_123", "screen_456"],
        "guaranteed_plays_per_day": 100,
        "time_slots": ["07:00-09:00", "17:00-19:00"],
        "days": ["mon", "tue", "wed", "thu", "fri"],
        "exclusivity": true
    }
}
```

DOOH PG deals are common: "exclusive 100 plays/day on these 2 screens during rush hour for 4 weeks."

#### DOOH Bid Modifiers

```yaml
bid_modifiers:
  venue_type:
    transit: +20%
    airport: +40%
    gas_station: -10%
  time_of_day:
    "07:00-09:00": +30%    # morning rush
    "17:00-19:00": +25%    # evening rush
    "10:00-16:00": +0%     # midday
    "22:00-06:00": -50%    # overnight (low foot traffic)
  weather:
    sunny: +10%            # more people outdoors
    rainy: -20%            # fewer people outdoors
    hot: +15%              # ice cream / beverage brands bid higher
  day_of_week:
    friday: +15%           # pre-weekend
    saturday: +10%
    sunday: -10%
```

#### DOOH Frequency Capping

DOOH frequency capping is **audience-level, not individual-level** since we can't identify individual viewers:

| Dimension | Example | How |
|---|---|---|
| Per screen per hour | "Max 6 plays per hour on this screen" | Exchange limits play frequency per screen |
| Per advertiser per screen per day | "Max 20 plays per day for Acme on screen X" | Exchange tracks advertiser plays per screen |
| Competitive separation per rotation | "No two car brands in the same 10-minute rotation" | Exchange checks recent plays |

No Redis frequency caps (no user identity). Instead, the Exchange tracks play history per screen in L1 cache.

#### DOOH Billing

| Model | Calculation |
|---|---|
| Cost per play | Advertiser pays per confirmed play on screen |
| CPM (audience-adjusted) | Cost per 1000 estimated impressions = plays * audience_per_play |
| Share of voice | Advertiser buys X% of all plays on a screen for a time period (deal-based) |

Billing uses proof-of-play confirmation from the screen. No play confirmation = no bill.

#### DOOH Reporting Metrics

| Metric | Calculation |
|---|---|
| Total plays | Count of confirmed plays |
| Estimated impressions | plays * estimated_audience_per_play |
| Audience reached (estimated) | Unique mobile devices detected near screens during plays |
| Share of voice | Advertiser's plays / total plays on screen |
| Cost per play | Total spend / total plays |
| eCPM | Total spend / (estimated impressions / 1000) |
| Foot traffic lift | % increase in advertiser store visits near screen location (from mobile data) |
| QR scan rate | QR scans / estimated impressions |
| Time slot performance | Plays and audience by hour of day |
| Weather impact | Performance correlation with weather conditions |

#### DOOH Fraud

| Fraud type | Detection |
|---|---|
| Fake proof-of-play | Screen reports playing but was off or malfunctioning. Cross-reference with uptime monitoring. |
| Audience inflation | Screen operator over-reports audience numbers. Validate against independent mobile device data. |
| Ghost screens | Screen registered but doesn't physically exist. Require photo verification + GPS confirmation during onboarding. |
| Low viewability | Screen facing a wall, behind obstacles. Site audit during onboarding. |

#### DOOH Quality Controls

```json
{
    "screen_id": "screen_123",
    "quality_controls": {
        "blocked_categories": ["IAB25", "IAB26"],
        "blocked_advertisers": ["adv_competitor"],
        "max_plays_per_advertiser_per_hour": 4,
        "require_content_approval": true,
        "min_dwell_time_seconds": 3
    }
}
```

#### DOOH in Publisher Simulator

New simulator template showing a mock digital billboard with rotation:

```
┌──────────────────────────────────────────┐
│  DOOH Simulator - Times Square Billboard │
│                                          │
│  ┌──────────────────────────────────┐   │
│  │                                  │   │
│  │     [Ad Creative Displays]       │   │
│  │     Currently: Acme Shoes        │   │
│  │     Slot: 10s / rotation: 6/min  │   │
│  │     trace: abc-123               │   │
│  │                                  │   │
│  └──────────────────────────────────┘   │
│                                          │
│  Rotation: [Acme 10s] [Nike 10s]        │
│            [BMW 10s] [Slate 10s]        │
│                                          │
│  Debug: Play confirmed ✓ | Audience: 147│
│  Weather: Sunny 22°C | Rush hour: Yes   │
│  [Next rotation] [Change venue] [Change  │
│   weather] [View trace →]               │
└──────────────────────────────────────────┘
```

#### DOOH Optimisation

| Pipeline | DOOH optimisation |
|---|---|
| Screen scoring | Which screens drive best audience/cost ratio for this advertiser? |
| Time slot optimisation | Which hours give best ROI? Shift budget to peak hours. |
| Weather-responsive | Auto-adjust bids based on weather forecast (ice cream brand bids up when hot) |
| Venue mix | Optimal mix of transit, outdoor, indoor screens for this campaign |

---

## Retail Media

### Overview

Ads on retailer websites and apps - sponsored product listings, banner ads on category pages, promoted search results. The fastest growing ad channel because retailers have **first-party purchase data** - they know exactly what people buy, making targeting incredibly precise.

**Why retail media is different:**

| Concern | Standard programmatic | Retail media |
|---|---|---|
| Inventory | Publisher websites/apps | Retailer's own website/app (Amazon, Tesco, Walmart) |
| Targeting data | Third-party cookies, audience segments | First-party purchase history (gold standard) |
| Ad formats | Display, video, native | Sponsored products, sponsored brands, display on retail pages |
| Attribution | Complex (view-through, click-through, cross-device) | Simple (user bought the product on the same site they saw the ad) |
| Conversion | Usually happens on a different site | Happens on the same site (closed loop) |
| CPMs | $2-20 | $1-15 (but ROAS is very high because targeting is precise) |

### Retail Media Ad Formats

| Format | What it looks like | Where it appears |
|---|---|---|
| **Sponsored Product** | Looks like an organic product listing with "Sponsored" label | Search results, category pages, product detail pages |
| **Sponsored Brand** | Brand banner with logo, tagline, and product carousel | Top of search results |
| **Display on retail** | Standard banner ad on retail pages | Homepage, category pages, checkout |
| **Video on retail** | Autoplay video on product pages or in search results | Product detail pages |

### How Retail Media Fits Our Platform

```
Retailer (publisher) integrates our platform:
    |
    v
Shopper searches "running shoes" on retailer's site
    |
    v
Retailer's page sends bid request to our SSP with:
    - Search query: "running shoes"
    - Page type: search_results
    - Shopper signals: purchase history, browsing history, loyalty tier
    |
    v
Exchange runs auction among shoe brand advertisers
    |
    v
Winning sponsored product promoted to top of search results
    |
    v
Shopper clicks -> product detail page (on same site)
    |
    v
Shopper buys -> conversion tracked instantly (closed-loop attribution)
    |
    v
Advertiser sees: ad spend -> sale -> exact ROAS, same day
```

### Retail-Specific Bid Request

```json
{
    "site": {
        "domain": "retailer.com",
        "page": "https://retailer.com/search?q=running+shoes",
        "cat": ["IAB22-2"]
    },
    "user": {
        "id": "shopper_123",
        "ext": {
            "loyalty_tier": "gold",
            "purchase_history_categories": ["sports", "footwear"],
            "basket_value": 45.00,
            "days_since_last_purchase": 12
        }
    },
    "imp": [{
        "id": "1",
        "ext": {
            "placement_type": "sponsored_product",
            "search_query": "running shoes",
            "page_type": "search_results",
            "category_id": "footwear_running",
            "position": "top_3",
            "max_products": 3
        }
    }]
}
```

### Retail-Specific Targeting

| Dimension | Example | Why |
|---|---|---|
| Search query / keywords | "running shoes", "protein powder" | Show relevant sponsored products |
| Product category | Footwear > Running > Men's | Category-level targeting |
| Purchase history | "Bought running shoes in last 90 days" | Retarget or suppress (don't advertise what they already bought) |
| Basket value | Current basket > $50 | Cross-sell to high-value shoppers |
| Loyalty tier | Gold, Silver, New | Different bid strategy per tier |
| Competitor conquesting | Shopper viewing a competitor's product page | "Show your product when they're looking at the competitor" |
| Days since last purchase | 30+ days | Re-engagement campaigns |

### Retail Billing Model

| Model | How it works | Use case |
|---|---|---|
| CPC (sponsored products) | Pay per click on sponsored listing | Most common for sponsored products |
| CPM (display on retail) | Pay per 1000 impressions on retail pages | Brand awareness on retail |
| CPA / ROAS target | Pay per sale, or target a ROAS | Performance campaigns with closed-loop attribution |

**Closed-loop attribution** is the key advantage: the conversion happens on the same site as the ad. No cross-site tracking needed, no attribution windows, no guessing. Click -> purchase on same site = conversion.

### Retail Media Integration with Existing Systems

| System | Retail media integration |
|---|---|
| Exchange | Same auction. Sponsored product auctions may rank by relevance * bid (not just bid). |
| DSP | Retailer provides first-party shopper signals. DSP uses them for bid evaluation. |
| Billing | CPC dominant. Closed-loop conversion tracking means ROAS is calculated instantly. |
| Reporting | Product-level performance: which SKU, which search query, which position. |
| Deals | Retailer may offer "always-on" sponsored product slots at fixed CPC. |
| Creative | Sponsored products use the product's own image/title/price from the retailer's catalog - not a custom creative. |
| Fraud | Click fraud on sponsored products (competitor clicking to drain budget). |

### Retail Media: What's Architecturally New

| Component | What's new | Reuse |
|---|---|---|
| **Product catalog sync** | Retailer syncs their product catalog so we know which products advertisers can promote | New - `pkg/retail/catalog.go` |
| **Relevance scoring** | Auction winner isn't just highest bid - product must be relevant to the search query | New - `pkg/retail/relevance.go` |
| **Keyword bidding** | Advertisers bid on search keywords (like search ads) | New concept for our platform |
| **Product-level targeting** | Target specific SKUs, categories, competitor products | Extension of existing targeting |
| **Shelf position** | Ad appears at position 1, 2, or 3 in search results - position affects CTR | New - position-based pricing |
| **Catalog creative** | Creative is auto-generated from product data (image, title, price, rating) | New - `pkg/retail/creative.go` |

### Implementation

| Component | Location |
|---|---|
| Product catalog sync | `pkg/retail/catalog.go` - retailer product feed ingestion |
| Relevance scoring | `pkg/retail/relevance.go` - query-to-product relevance |
| Keyword bidding | `pkg/retail/keywords.go` - keyword targeting and bid management |
| Catalog creative generation | `pkg/retail/creative.go` - auto-generate ad from product data |
| Retail bid request | `pkg/openrtb/retail.go` - retail-specific imp extensions |
| Retail reporting | `pkg/reporting/retail.go` - product-level, keyword-level, ROAS reporting |

### Retail Media Cross-System Integration Detail

#### Retail Creative Review

Sponsored products don't use traditional creatives - they use the **retailer's product catalog data**:

| Check | Requirement |
|---|---|
| Product exists in catalog | SKU must be active in retailer's feed |
| Product in stock | Out-of-stock products can't be promoted (checked at serve time) |
| Product image quality | Min 500x500, white background, no watermarks (retailer's standards) |
| Title length | Max 150 chars (retailer's template) |
| Price accuracy | Price in ad must match current catalog price |
| Category match | Product must be in a category the advertiser owns |
| Landing URL | Must link to the product page on the retailer's site |

For sponsored brand (banner format), standard creative review applies (image dimensions, content policy).

#### Retail Deals

```json
{
    "deal_id": "deal_retail_321",
    "deal_type": "preferred",
    "format": "sponsored_product",
    "retail_config": {
        "retailer_id": "retailer_456",
        "categories": ["footwear_running"],
        "position": "top_3",
        "fixed_cpc": 0.75,
        "always_on": true,
        "budget_daily": 500.00,
        "product_skus": ["SKU_001", "SKU_002", "SKU_003"]
    }
}
```

"Always-on" deals are common in retail: advertiser's products are always promoted at a fixed CPC as long as daily budget remains.

#### Retail Bid Modifiers

```yaml
bid_modifiers:
  page_type:
    search_results: +0%
    category_page: -20%
    product_detail_page: +30%     # competitor conquesting
    checkout: +50%                 # last chance to cross-sell
    homepage: -10%
  position:
    position_1: +40%              # top result
    position_2: +15%
    position_3: +0%
  shopper_signals:
    loyalty_gold: +25%            # high-value shoppers
    repeat_buyer: +20%            # already bought this brand before
    competitor_viewer: +35%       # viewing a competitor product
  time:
    "black_friday": +100%         # peak shopping
    "cyber_monday": +80%
```

#### Retail Frequency Capping

| Dimension | Example | Redis key |
|---|---|---|
| Per shopper per product per day | "Don't show this SKU more than 3 times per day to same shopper" | `fc:{shopper}:{sku}:d` |
| Per shopper per advertiser per session | "Max 5 sponsored products from same brand per shopping session" | `fc:{shopper}:{advertiser}:session` |
| Per shopper per category | "Max 2 sponsored products in running shoes per page" | Enforced at serve time by Exchange |

#### Retail Reporting Metrics

| Metric | Calculation |
|---|---|
| Impressions | Sponsored product shown in results |
| Clicks | Shopper clicked the sponsored listing |
| CTR | Clicks / impressions |
| Purchases (attributed) | Shopper bought the promoted product (same session or within window) |
| Conversion rate | Purchases / clicks |
| Revenue (attributed) | Total revenue from attributed purchases |
| ROAS | Revenue / ad spend |
| ACoS (advertising cost of sales) | Ad spend / revenue (inverse of ROAS, common in retail) |
| New-to-brand purchases | First-time buyers of this brand (high value) |
| Keyword performance | Impressions, clicks, conversions per search keyword |
| Position performance | CTR and conversion rate by position (1st, 2nd, 3rd) |
| Share of voice | % of impressions in a category won by this advertiser |
| Basket impact | Average basket value when sponsored product is in cart vs not |

#### Retail Fraud

| Fraud type | Detection |
|---|---|
| Competitor click fraud | Competitor clicking sponsored products to drain budget. Rate limit per IP/user, statistical anomaly detection on click patterns. |
| Click injection | Fake clicks from bots. Device attestation, click-to-purchase ratio analysis (if clicks never lead to purchases = suspicious). |
| Fake purchases | Fraudulent orders to inflate ROAS. Cross-reference with retailer's order cancellation/return data. |
| Keyword stuffing | Advertiser promoting products on irrelevant keywords. Relevance scoring filters these out. |

#### Retail Quality Controls

```json
{
    "retailer_id": "retailer_456",
    "retail_quality_controls": {
        "max_sponsored_per_page": 4,
        "max_sponsored_per_category_page": 3,
        "min_relevance_score": 0.3,
        "blocked_advertisers": [],
        "require_in_stock": true,
        "price_match_tolerance_pct": 1,
        "organic_ratio": 0.7
    }
}
```

`organic_ratio: 0.7` means at least 70% of results must be organic (not sponsored). Prevents the search results from feeling like all ads.

#### Retail in Publisher Simulator

```
┌──────────────────────────────────────────────────┐
│  Retail Simulator - ShopExample.com               │
│                                                    │
│  Search: [running shoes          ] [Search]        │
│                                                    │
│  ┌─ Sponsored ──────────────────────────────┐     │
│  │ ⓢ Acme UltraRun Pro - $89.99 ★★★★½     │     │
│  │   "Lightweight racing shoe" | Free shipping│    │
│  │   trace: abc-123 [View trace →]           │     │
│  └───────────────────────────────────────────┘     │
│  ┌─ Sponsored ──────────────────────────────┐     │
│  │ ⓢ SpeedX Runner - $79.99 ★★★★          │     │
│  │   "All-terrain running shoe"              │     │
│  │   trace: def-456 [View trace →]           │     │
│  └───────────────────────────────────────────┘     │
│                                                    │
│  Organic Results:                                  │
│  Nike Air Zoom - $119.99 ★★★★★                   │
│  Adidas Ultraboost - $109.99 ★★★★½               │
│  ...                                               │
│                                                    │
│  Debug: 2 sponsored products | 3 bids received     │
│  Relevance scores: Acme 0.85, SpeedX 0.72         │
│  Winning formula: bid * relevance                  │
│  [Change search query] [Change shopper profile]    │
└──────────────────────────────────────────────────┘
```

#### Retail Optimisation

| Pipeline | Retail optimisation |
|---|---|
| Keyword optimisation | Which keywords drive best ROAS? Auto-adjust bids per keyword. |
| Product-level performance | Which SKUs convert best when sponsored? Recommend promoting top converters. |
| Position analysis | Position 1 has highest CTR but highest CPC. Is position 2 more profitable? |
| Daypart optimisation | Shopping peaks at lunch and evening. Shift budget to peak hours. |
| Cross-sell recommendations | "Shoppers who buy running shoes also buy running socks. Promote socks on shoe purchase confirmation page." |
| Competitor intelligence | "Competitor's CPC on 'running shoes' increased 20% this week." |

---

## In-Game Advertising

### Overview

Ads inside video games - on virtual billboards, loading screens, menu screens, and as rewarded ads (watch an ad, get an in-game reward). Growing fast as gaming audiences rival TV viewership.

**In-game ad formats:**

| Format | What it looks like | Intrusiveness | CPMs |
|---|---|---|---|
| **Intrinsic / blended** | Ad on a virtual billboard, stadium banner, or poster inside the game world | Very low - feels natural | $10-20 |
| **Interstitial** | Full-screen ad between game levels or during loading | High | $15-30 |
| **Rewarded** | Player chooses to watch a 15-30s video ad in exchange for in-game reward | Medium (opt-in) | $20-40 |
| **Banner** | Small banner overlay during gameplay | Low-medium | $5-15 |
| **Audio** | Audio ad during loading or menu screens | Low | $8-15 |
| **Playable** | Mini-game ad (try the advertised game for 15s) | Medium (interactive) | $30-50 |

### How In-Game Fits Our Platform

```
Game developer (publisher) integrates our SDK:
    |
    v
Player reaches an ad opportunity (loading screen, billboard, reward button):
    |
    v
Game SDK sends bid request to our SSP:
    {game_bundle, genre, player_level, session_duration, ad_format, device}
    |
    v
Exchange runs auction
    |
    v
Winning creative returned to SDK:
    - Interstitial/rewarded: VAST video or display creative
    - Intrinsic: texture/image for the in-game billboard
    |
    v
SDK renders the ad in-game, tracks viewability, reports events
    |
    v
Rewarded: SDK verifies completion -> grants reward -> server-to-server callback
```

### Rewarded Ads (Unique Mechanic)

Player actively chooses to watch an ad. Highest engagement format in gaming.

```
Player runs out of lives in a game
    |
    v
Game shows: "Watch a short video to earn an extra life?"
    [Watch Ad]  [No Thanks]
    |
    v
Player taps [Watch Ad]
    |
    v
SDK requests VAST video from our Ad Server (30s, non-skippable)
    |
    v
Video plays to completion
    |
    v
SDK fires: completion beacon -> our Tracker records complete event
    |
    v
SDK calls game server: "grant reward to player_abc"
    Game server calls our platform: POST /v1/t/reward?tid={trace_id}&reward_type=extra_life
    Our platform verifies: yes, trace_id had a valid completion event
    Response: {verified: true}
    |
    v
Game grants reward. Player happy. Advertiser got 30s of full attention.
```

**Server-to-server reward verification** prevents fraud (player faking completion without watching):

```
Game server -> POST /v1/t/reward?tid=abc&sig=xyz
    |
    v
Tracker checks:
    1. Is trace_id valid? -> Yes
    2. Was completion event received for this trace? -> Yes
    3. Was it received recently (within 5 min)? -> Yes
    4. Was it already redeemed? -> No (prevent double-redeem)
    |
    v
Response: {verified: true, reward_id: "rwd_123"}
    |
    v
Game server grants reward, records reward_id to prevent replay
```

### OpenRTB In-Game Bid Request

```json
{
    "app": {
        "bundle": "com.example.racingGame",
        "name": "Street Racing Pro",
        "cat": ["IAB9-30"],
        "storeurl": "https://play.google.com/store/apps/details?id=com.example.racingGame",
        "ver": "3.2.1"
    },
    "device": {
        "devicetype": 4,
        "make": "Samsung",
        "model": "Galaxy S24",
        "os": "Android",
        "osv": "15"
    },
    "imp": [{
        "id": "1",
        "video": {
            "mimes": ["video/mp4"],
            "minduration": 15,
            "maxduration": 30,
            "skip": 0,
            "w": 1920,
            "h": 1080
        },
        "ext": {
            "placement_type": "rewarded",
            "reward_type": "extra_life",
            "reward_amount": 1,
            "game_genre": "racing",
            "player_level": 45,
            "session_duration_minutes": 23,
            "in_app_purchases_last_30d": 2
        }
    }]
}
```

### In-Game Specific Targeting

| Dimension | Example | Why |
|---|---|---|
| Game genre | racing, puzzle, rpg, casual, sports | Different audiences per genre |
| Player level / engagement | Level 45, daily player, 23min session | High-level = engaged = higher value |
| IAP history | 2 purchases in 30 days | Paying players = high LTV |
| Session duration | 23 minutes in current session | Longer session = more engaged |
| Ad format | rewarded, interstitial, intrinsic | Different creative requirements |
| Device tier | High-end vs budget device | Rich media capability |
| Network type | wifi vs cellular | Video quality adaptation |
| Age rating | PEGI 3, PEGI 12, PEGI 18 | Ad content must match game age rating |

### Intrinsic / Blended Ads (In-Game Billboards)

Ads that exist naturally within the game world:

```
Racing game: billboards along the track show real ads
Sports game: stadium banners show real brand ads
Open world: bus shelters and posters show real ads
```

**How intrinsic ads work:**

```
Game loads a level with 3 billboard placements:
    |
    v
SDK batch-requests 3 ads: POST to SSP with 3 imp objects
    |
    v
Exchange runs 3 auctions
    |
    v
SDK receives 3 creative textures (images)
    |
    v
Game engine applies textures to billboard objects in the scene
    |
    v
Viewability: SDK tracks if player's camera faced the billboard for >1 second
    -> Yes: impression counted
    -> No: not counted (player drove past too fast or never looked at it)
```

**Viewability for intrinsic ads** is unique - the game SDK must track the player's camera angle and distance from the billboard. An ad on a billboard behind the player doesn't count.

### In-Game Billing Models

| Model | What advertiser pays for | Use case |
|---|---|---|
| CPCV (rewarded) | Per completed view (watched to 100%) | Rewarded ads (non-skippable) |
| CPM (interstitial) | Per 1000 impressions | Loading screen ads |
| CPM (intrinsic) | Per 1000 viewable impressions (player looked at it) | In-game billboards |
| CPI (cost per install) | Per app install from the ad | Game cross-promotion |

### In-Game Cross-System Integration Detail

#### In-Game Creative Review

| Check | Requirement |
|---|---|
| Age rating match | Ad content must be appropriate for the game's age rating (PEGI/ESRB). Mature ad in PEGI 3 game = rejected. |
| Format match | Rewarded/interstitial: VAST video (same as standard video validation). Intrinsic: texture image (PNG/JPEG). |
| Video duration | Rewarded: 15s or 30s (non-skippable). Interstitial: 15s or 30s (skippable after 5s). |
| Texture dimensions | Intrinsic: must match billboard aspect ratio (16:9, 4:3, 1:1 depending on game) |
| File size (texture) | Max 2MB per texture (games are memory-constrained) |
| Animation | Intrinsic textures must be static (no animated GIFs - game engine renders them as textures) |
| Content policy | No competing game ads in some publisher deals. No misleading "fake UI" ads (fake close buttons, fake gameplay). |

#### In-Game Deals

```json
{
    "deal_id": "deal_game_999",
    "deal_type": "pmp",
    "format": "rewarded",
    "game_config": {
        "game_bundles": ["com.example.racingGame"],
        "placement_type": "rewarded",
        "min_player_level": 10,
        "min_session_duration_minutes": 5,
        "exclusive_category": "automotive",
        "guaranteed_completions_daily": 5000
    }
}
```

Game publishers often sell exclusive rewarded ad slots: "only automotive brands can show rewarded ads in our racing game."

#### In-Game Bid Modifiers

```yaml
bid_modifiers:
  game_genre:
    racing: +15%            # racing game = good for automotive
    puzzle: +0%
    rpg: +10%               # long sessions, engaged players
    casual: -10%            # short sessions, lower engagement
  player_engagement:
    level_50_plus: +30%     # experienced player, likely to stay
    new_player: -20%        # might churn
    daily_player: +25%      # reliable audience
  iap_history:
    has_purchased: +40%     # proven spender
    no_purchases: +0%
  ad_format:
    rewarded: +20%          # opt-in, highest engagement
    interstitial: +0%
    intrinsic: -10%         # viewability uncertain
  session_duration:
    "30_plus_minutes": +15% # deeply engaged
    "under_5_minutes": -30% # barely playing
```

#### In-Game Frequency Capping

| Dimension | Example | Redis key |
|---|---|---|
| Per player per game session | "Max 3 rewarded ads per session" | `fc:{player}:{game}:session:{session_id}` |
| Per player per day | "Max 8 rewarded ads per day across all games" | `fc:{player}:rewarded:d` |
| Per player per advertiser per day | "Max 2 ads from Acme per day" | `fc:{player}:{advertiser}:game:d` |
| Per player per creative | "Don't show same creative more than once per session" | `fc:{player}:{creative}:{session_id}` |
| Cooldown between ads | "Min 5 minutes between interstitial ads" | `fc:{player}:interstitial:cooldown` TTL 5min |

The cooldown cap is important in gaming - showing ads too frequently causes player churn. Game publishers set minimum intervals.

#### In-Game Billing

| Model | Event source | When billable |
|---|---|---|
| CPCV (rewarded) | `adtech.events.video` with event_type=`complete` | Player watched to 100% (non-skippable) |
| CPM (interstitial) | `adtech.events.video` with event_type=`start` | Ad started playing |
| CPM (intrinsic viewable) | `adtech.events.ingame` with event_type=`viewable` | Player camera faced billboard for 1+ second |
| CPC | `adtech.events.click` | Player tapped/clicked the ad |
| CPI (cost per install) | `adtech.events.install` | Player installed the advertised app (postback from app store / MMP) |

**CPI flow (app install campaigns):**

```
Player sees ad for "PuzzleGame Pro" in RacingGame
    |
    v
Player clicks -> opens app store listing
    |
    v
Player installs PuzzleGame Pro
    |
    v
PuzzleGame Pro's SDK fires install postback to our platform:
    POST /v1/t/install?tid={trace_id}&app_bundle=com.puzzlegame.pro&sig=xyz
    |
    v
Tracker verifies postback signature
    -> Valid: record install event, settle CPI billing
    -> Invalid: reject (prevents fraudulent install claims)
```

#### In-Game Reporting Metrics

| Metric | Calculation |
|---|---|
| Completion rate (rewarded) | complete events / start events (typically 95%+ since non-skippable) |
| Opt-in rate (rewarded) | Users who chose "Watch Ad" / users who saw the prompt |
| Reward redemption rate | Rewards granted / completion events (should be ~100%, lower indicates verification issues) |
| Interstitial skip rate | Skip events / start events |
| Intrinsic viewability rate | Viewable impressions / total impressions (camera-based) |
| Average view duration (intrinsic) | How long players look at in-game billboards |
| CPI (effective) | Total spend / installs |
| Install-to-purchase rate | IAP revenue from installed users / installs |
| Player retention impact | Does seeing ads correlate with lower retention? (publisher metric) |
| Session depth at ad | Average session time when ad was shown (are we showing ads too early?) |
| Revenue per DAU | Total ad revenue / daily active users (publisher metric) |

#### In-Game Fraud

| Fraud type | Detection |
|---|---|
| Reward farming bots | Automated players that complete rewarded ads at scale. Detect via: device attestation (SafetyNet/App Attest), impossible play patterns, no real game progress. |
| Emulator fraud | Ads shown on emulators, not real devices. Detect via: device fingerprinting, emulator detection signals. |
| Click injection | Fake clicks injected by malware before real app installs (steals CPI attribution). Detect via: click-to-install time analysis (< 10 seconds = suspicious). |
| SDK spoofing | Fake SDK calls that simulate ad events without displaying ads. Detect via: SDK signature verification, server-to-server validation. |
| Install fraud | Fake installs from device farms. Detect via: post-install engagement analysis (real users engage, fake ones don't). |
| Viewability fraud (intrinsic) | Reporting billboard viewability when the billboard is hidden or off-screen. SDK integrity checks. |

#### In-Game Quality Controls

```json
{
    "game_bundle": "com.example.racingGame",
    "game_quality_controls": {
        "age_rating": "PEGI_7",
        "blocked_categories": ["IAB25", "IAB26", "IAB8-5"],
        "blocked_advertisers": ["adv_competitor_game"],
        "max_rewarded_per_session": 5,
        "max_interstitial_per_session": 3,
        "min_interval_between_interstitials_seconds": 300,
        "rewarded_exclusive_categories": ["automotive", "entertainment"],
        "intrinsic_billboard_count": 8,
        "intrinsic_refresh_interval_seconds": 60
    }
}
```

`intrinsic_refresh_interval_seconds: 60` means in-game billboards change ads every 60 seconds during gameplay, creating multiple impression opportunities per session.

#### In-Game in Publisher Simulator

```
┌──────────────────────────────────────────────────┐
│  In-Game Simulator - Racing Game                  │
│                                                    │
│  ┌──────────────────────────────────────────┐     │
│  │                                          │     │
│  │  [Game Scene with Billboard]             │     │
│  │                                          │     │
│  │  Billboard: Acme Shoes                   │     │
│  │  Viewable: Yes (camera facing, 2.3s)     │     │
│  │  trace: abc-123                          │     │
│  │                                          │     │
│  └──────────────────────────────────────────┘     │
│                                                    │
│  ┌─ Rewarded Ad Prompt ─────────────────────┐    │
│  │ Watch a 30s ad for an extra life?         │    │
│  │ [Watch Ad]  [No Thanks]                   │    │
│  │ trace: def-456                            │    │
│  └───────────────────────────────────────────┘    │
│                                                    │
│  Debug:                                            │
│  Intrinsic: 3 billboards loaded, 2 viewable       │
│  Rewarded: prompted, awaiting choice               │
│  Session: 23min, level 45, 1 rewarded viewed      │
│  [Trigger rewarded] [Rotate billboards]            │
│  [Change game genre] [View trace →]               │
└──────────────────────────────────────────────────┘
```

#### In-Game Optimisation

| Pipeline | In-game optimisation |
|---|---|
| Reward placement timing | When in the game session should we show rewarded prompts? Too early = player hasn't invested enough to care about reward. Too late = player may have left. |
| Interstitial frequency | Optimal number of interstitials per session that maximises revenue without causing churn. |
| Intrinsic refresh rate | How often to rotate billboard ads. Too frequent = wasted impressions (player not looking). Too slow = missed opportunities. |
| Game genre matching | Which ad categories perform best in which game genres? (Automotive in racing, food/bev in casual) |
| Player value segmentation | High IAP players vs free players. Bid differently for each. |
| Creative format testing | Does a 15s or 30s rewarded ad drive better CPCV for this advertiser? |

### NATS Subjects for New Channels

| Subject | Publisher | Subscribers | Payload |
|---|---|---|---|
| `adtech.events.dooh` | Screen proof-of-play | Reporting (analytics + billing) | DOOHEvent{screen_id, plays, estimated_audience} |
| `adtech.events.retail` | Retail site tracker | Reporting (analytics + billing) | RetailEvent{product_id, query, position, conversion} |
| `adtech.events.reward` | Game SDK via Tracker | Reporting (analytics + billing) | RewardEvent{trace_id, reward_type, verified} |

### Implementation

| Component | Location |
|---|---|
| **DOOH** | |
| OpenRTB DOOH object | `pkg/openrtb/dooh.go` |
| DOOH targeting | `pkg/targeting/dooh.go` |
| Audience estimation | `pkg/dooh/audience.go` |
| Proof-of-play tracker | Tracker: `POST /v1/t/dooh` |
| **Retail Media** | |
| Product catalog sync | `pkg/retail/catalog.go` |
| Relevance scoring | `pkg/retail/relevance.go` |
| Keyword bidding | `pkg/retail/keywords.go` |
| Catalog creative | `pkg/retail/creative.go` |
| **In-Game** | |
| Reward verification | Tracker: `GET /v1/t/reward` |
| Intrinsic ad serving | Ad Server: texture/image delivery for game billboards |
| Game SDK viewability | Documented API for game SDK integration |

### For MVP

These channels are deferred but designed for:

1. **DOOH Phase 1:** OpenRTB `dooh` object, screen registration as placement type, proof-of-play tracking, location-based targeting
2. **Retail Media Phase 1:** Product catalog sync, keyword bidding, sponsored product auction (relevance * bid), closed-loop conversion tracking
3. **In-Game Phase 1:** Rewarded ad VAST serving, server-to-server reward verification endpoint, intrinsic ad texture serving

All three channels use the same Exchange, same billing, same reporting infrastructure. The new code is primarily in targeting (new dimensions), creative formats (textures, catalog products), and measurement (proof-of-play, reward verification, camera-based viewability).

---

## Unified Audience Management Layer

### Overview

Audience data comes from many sources - publisher browsing data, advertiser CRM uploads, platform tracking, CDP integrations. This layer unifies all audience data into a single system that the DSP uses for targeting.

**Data sources flowing in:**

```
Publisher first-party data (ad tag)
    |                                       ┌─────────────────────────────┐
Advertiser CRM uploads                      │                             │
    |                                       │   Unified Audience Layer    │
Platform tracking (impressions, clicks)     │                             │
    |                                       │   pkg/audience/             │
Identity graph (cross-device/publisher)  -->│   pkg/identity/             │--> DSP targeting
    |                                       │   pkg/privacy/              │--> Reporting
CDP connectors (Segment, mParticle)         │                             │--> Optimisation
    |                                       │   Storage:                  │
Conversion data (post-click, post-view)     │   Postgres (segments)       │
    |                                       │   Redis (fast lookups)      │
Offline data (purchase history uploads)     │   Analytics (audience size) │
                                            └─────────────────────────────┘
```

### Audience Segment Types

| Type | Source | Example | How it's created |
|---|---|---|---|
| **First-party publisher** | Publisher ad tag sends user attributes | "subscriber", "sports_reader", "age_25_34" | Publisher passes via `adtech.js` |
| **First-party advertiser** | CRM upload (hashed emails + segments) | "existing_customer", "high_value", "lapsed" | Upload via `/v1/api/audiences/upload` |
| **Platform behavioural** | Our own tracking across the platform | "clicked_car_ads_last_7d", "visited_travel_sites" | Auto-generated from event data |
| **Lookalike** | ML model finds users similar to a seed audience | "looks_like_high_value_customers" | Built by optimisation pipeline |
| **Suppression** | "Do NOT target" lists | "existing_customers" (acquisition campaigns) | Upload with `suppression: true` |
| **Retargeting** | Users who performed a specific action | "visited_product_page_but_didnt_buy" | Auto-generated from conversion/click data |
| **Contextual** | Derived from page content at bid time, not from user history | "reading_about_cars_right_now" | Real-time classification at SSP |
| **Composite** | Boolean combination of other segments | "sports_fan AND high_value AND NOT existing_customer" | Built in segment builder UI |
| **Predictive** | ML-predicted attributes | "likely_to_convert", "likely_to_churn" | Built by optimisation pipeline |
| **CDP imported** | Synced from external CDPs | "mParticle: active_app_users" | CDP connector |

### Lookalike Audiences

Advertisers have a seed audience (e.g. "my best 1000 customers"). Lookalike modeling finds more users who behave similarly.

```
Advertiser uploads seed: 1000 hashed emails of best customers
    |
    v
Audience layer matches seeds to platform identity graph:
    800 of 1000 matched to platform users
    |
    v
ML model analyzes matched users:
    - Common attributes: age 25-34, reads sports, mobile user, clicks car ads
    - Browsing patterns: visits in evening, 3+ sessions per week
    - Purchase signals: high basket value, uses discount codes
    |
    v
Score all platform users against this profile:
    - Score > 0.8: "top 1% lookalike" (very similar)
    - Score > 0.6: "top 5% lookalike"
    - Score > 0.4: "top 10% lookalike"
    |
    v
Result: Lookalike audience segment with ~500,000 users at top 5%
    Advertiser can target this segment in line item targeting
```

**API:**
- `POST /v1/api/audiences/lookalike` - create lookalike from seed audience
- Body: `{seed_audience_id: "aud_123", expansion: "5%", name: "LAL - Best Customers"}`

### Composite Segments (Boolean Logic)

Advertisers build complex segments from existing ones using AND/OR/NOT:

```json
{
    "segment_name": "High Value Acquisition",
    "logic": {
        "AND": [
            {"segment": "in_market_cars"},
            {"segment": "high_income"},
            {"OR": [
                {"segment": "sports_fan"},
                {"segment": "tech_enthusiast"}
            ]},
            {"NOT": {"segment": "existing_customer"}},
            {"NOT": {"segment": "recently_converted_30d"}}
        ]
    }
}
```

Evaluated at bid time by the DSP targeting engine. Composite segments don't pre-compute membership - they evaluate the boolean expression against the user's segments in real-time.

### Audience Analytics

Dashboard for understanding audience composition and planning:

| Feature | What it shows |
|---|---|
| Segment size | Estimated unique users in a segment (HyperLogLog) |
| Segment overlap | "How much do sports_fans and car_enthusiasts overlap?" (HLL intersection) |
| Audience composition | Demographics, geo, device breakdown of a segment |
| Segment growth | Is this segment growing or shrinking over time? |
| Reachable audience | "How many users in this segment have we seen in the last 7 days?" (can actually serve them ads) |
| Segment comparison | Side-by-side metrics for two segments |

**API:**
- `GET /v1/api/audiences/{id}/analytics` - composition, size, growth
- `GET /v1/api/audiences/overlap?ids=aud_1,aud_2` - overlap analysis
- `GET /v1/api/audiences/{id}/reachable?days=7` - active reachable users

### Data Collection Pixels

Advertisers place pixels on their website to collect data for retargeting and conversion tracking:

```html
<!-- Advertiser places on their product page -->
<img src="https://tracker.example.com/v1/t/data?aid=adv_123&event=product_view&product_id=SKU_456&sig=xyz" width="1" height="1">

<!-- Advertiser places on their checkout confirmation page -->
<img src="https://tracker.example.com/v1/t/data?aid=adv_123&event=purchase&value=89.99&sig=xyz" width="1" height="1">
```

These create retargeting segments automatically:
- `product_view:SKU_456` -> "viewed this product, didn't buy"
- `purchase` -> "existing customer" (suppression list)
- `cart_abandon` -> "added to cart but didn't buy" (high-intent retargeting)

**Tracker endpoint:** `GET /v1/t/data?aid={advertiser_id}&event={event_type}&sig={signature}` -> 1x1 pixel

**Pixel management UI:**
- `GET /v1/api/pixels` - list advertiser's data collection pixels
- `POST /v1/api/pixels` - create new pixel (event type, parameters)
- `GET /v1/api/pixels/{id}/code` - get the HTML/JS snippet to install
- `GET /v1/api/pixels/{id}/status` - is the pixel firing? Last seen, events/day

### CDP Connectors (Deferred but Designed)

Integration points for external Customer Data Platforms:

```
External CDP (Segment, mParticle, Tealium)
    |
    v
Webhook / API push: user segment updates in real-time
    POST /v1/api/cdp/webhook/{connector_id}
    Body: {user_id: "hashed_email_abc", segments_added: ["active_app_user"], segments_removed: ["lapsed"]}
    |
    v
Audience layer updates user's segments
    -> Available for targeting immediately
```

Connectors are configured per advertiser account:
- `POST /v1/api/cdp/connectors` - register a CDP connector (type, webhook URL, auth)
- Supported: Segment (webhook), mParticle (webhook), custom (generic webhook format)

### Data Taxonomy

All audience segments are tagged with IAB Audience Taxonomy categories for standardisation:

| IAB Category | Example segments |
|---|---|
| IAB-1 (Arts & Entertainment) | movie_fans, music_enthusiasts |
| IAB-4 (Careers) | job_seekers, recruiters |
| IAB-7 (Health & Fitness) | gym_goers, health_conscious |
| IAB-17 (Sports) | sports_fans, football_enthusiasts |
| IAB-19 (Technology) | tech_enthusiasts, early_adopters |

Taxonomy enables cross-platform segment matching and standardised reporting.

### Privacy in Audience Management

| Rule | Enforcement |
|---|---|
| Segments don't contain PII | Only hashed IDs and segment labels |
| Advertiser data isolation | Advertiser A's CRM data is NEVER visible to Advertiser B (RLS enforced) |
| Publisher data isolation | Publisher segments not shared with other publishers |
| Consent required | Behavioural segments only built for consented users |
| Opt-out removes from all segments | Level 1 opt-out removes from behavioural segments. Level 3 deletes all segment membership. |
| Retention | Segment membership expires after 90 days of inactivity (configurable) |
| Suppression lists override | If a user is in both a targeting segment AND a suppression list, suppression wins |

### Unified Audience Store

The single source of truth for "what do we know about this user?" All audience data from every source is normalised into one store with access controls baked in.

#### Why Unified

Without it, a bid evaluation requires 7+ lookups across different systems. With it, one lookup returns everything the requesting account is allowed to see.

#### Data Model

Every piece of audience data is stored as a **user-segment membership** with source and access metadata:

```
User "uuid-123" belongs to:
    ┌──────────────────────┬──────────────────────────┬─────────────────┬─────────┐
    │ Segment              │ Source                    │ Access          │ Expires │
    ├──────────────────────┼──────────────────────────┼─────────────────┼─────────┤
    │ sports_fan           │ platform:behavioral       │ all             │ 90d     │
    │ clicked_car_ads_7d   │ platform:behavioral       │ all             │ 7d      │
    │ likely_to_convert    │ platform:predictive        │ all             │ 24h     │
    │ high_value           │ crm:adv_456               │ owner:adv_456   │ none    │
    │ existing_customer    │ crm:adv_456               │ owner:adv_456   │ none    │
    │ subscriber           │ pub_fp:pub_789            │ owner:pub_789   │ session │
    │ age_25_34            │ pub_fp:pub_789            │ owner:pub_789   │ session │
    │ uk_sports            │ marketplace:pub_789       │ purchased:adv_456│ 30d    │
    │ in_market_auto       │ barter:pub_321            │ barter:adv_456  │ 60d    │
    │ lal_best_customers   │ lookalike:adv_456         │ owner:adv_456   │ 30d    │
    │ visited_acme_shoes   │ retarget:adv_456          │ owner:adv_456   │ 30d    │
    │ mparticle_active     │ cdp:adv_456:mparticle     │ owner:adv_456   │ sync    │
    └──────────────────────┴──────────────────────────┴─────────────────┴─────────┘
```

#### Access Control Rules

When the DSP queries `GetSegments(user_id, requester_account_id)`, the store filters by access:

| Access type | Who can see it | Example |
|---|---|---|
| `all` | Any account on the platform | Platform behavioral and predictive segments |
| `owner:{account_id}` | Only the account that created/uploaded it | Advertiser CRM data, publisher first-party data |
| `purchased:{account_id}` | Account that purchased marketplace access | Marketplace data buyer |
| `barter:{account_id}` | Account in an active barter agreement | Both barter parties |
| `deal:{deal_id}` | Accounts participating in a specific deal | PMP deal with shared audience data |

**The store NEVER returns segments the requester doesn't have access to.** This is enforced at the store layer, not the application layer - defence in depth similar to Postgres RLS.

#### Storage Architecture

```
Write path (ingest):                      Read path (bid evaluation):

Publisher ad tag        ─┐                Bid request arrives
Advertiser CRM upload   ─┤                    |
Platform events         ─┤── Normalize ──>  [Redis: Audience Store]  ──> DSP targeting
Marketplace activation  ─┤    & write         (user_id -> segments)
Barter activation       ─┤
CDP connector           ─┤                Read: single GET, <2ms
Lookalike pipeline      ─┤                All access filtering done in Redis
Retargeting builder     ─┘
                                          Fallback: Postgres (if Redis miss)
```

**Redis structure:**

```
Key:   audience:{user_id}
Value: Hash map
    {segment_id}:{source} -> {access_json, expires_at}

Example:
    audience:uuid-123
        sports_fan:platform:behavioral -> {"access":"all","exp":1719792000}
        high_value:crm:adv_456 -> {"access":"owner:adv_456","exp":0}
        uk_sports:marketplace:pub_789 -> {"access":"purchased:adv_456","exp":1721692800}
```

**Why Redis:**
- Single-digit millisecond reads (bid evaluation hot path)
- Hash maps support efficient per-user segment storage
- TTL per key handles expiry automatically
- Memory-efficient for millions of users with sparse segments

**Postgres as source of truth:**
- All segment memberships are also written to Postgres (async via NATS)
- Redis is rebuilt from Postgres on cold start
- Postgres supports complex queries for analytics (segment size, overlap, composition)
- Redis is the fast read path, Postgres is the durable write path

#### Write Path: How Data Gets In

Every data source writes through a normalisation layer that converts source-specific formats into the unified model:

```
Source-specific event arrives
    |
    v
pkg/audience/store/ingest.go:
    1. Validate: is this a known segment? Is the source authorized?
    2. Normalize: convert source format to unified model
    3. Access tag: attach the correct access rules
    4. Expiry: set TTL based on source type
    5. Write to Redis (immediate, for targeting)
    6. Publish to NATS: adtech.audience.membership_updated
    7. Postgres consumer writes durably (async)
```

| Source | Trigger | Normalisation |
|---|---|---|
| Publisher first-party | Ad tag fires with user attributes | Convert publisher's field names to platform segments |
| Advertiser CRM | Upload API + periodic sync | Hash matching, segment assignment per upload config |
| Platform behavioral | Event pipeline (impression/click/conversion) | Rule-based segment assignment from event patterns |
| Platform predictive | Optimisation pipeline (daily) | ML scores converted to segment membership |
| Lookalike | Lookalike pipeline (on-demand) | Score threshold -> segment membership |
| Retargeting | Data collection pixel fires | Event type -> retargeting segment (e.g. product_view -> "viewed_product_X") |
| Marketplace purchase | Marketplace purchase event | Activate provider's segment for buyer's access |
| Barter activation | Barter acceptance event | Activate both parties' segments for each other |
| CDP connector | Webhook from external CDP | Map external segment names to platform segments |

#### Read Path: How the DSP Uses It

During bid evaluation, the DSP makes one call:

```go
// pkg/audience/store/read.go
func (s *Store) GetSegments(ctx context.Context, userID string, requesterID string) ([]Segment, error) {
    // 1. Read all segments for user from Redis
    raw := s.redis.HGetAll("audience:" + userID)

    // 2. Filter by access control
    allowed := []Segment{}
    for segKey, accessJSON := range raw {
        access := parseAccess(accessJSON)
        if access.IsAllowed(requesterID) {
            allowed = append(allowed, parseSegment(segKey, access))
        }
    }

    // 3. Filter expired
    now := time.Now()
    active := filter(allowed, seg.ExpiresAt == 0 || seg.ExpiresAt > now.Unix())

    return active, nil
}
```

**Performance:** single Redis HGETALL + in-memory filtering. Target: <2ms for 99th percentile.

#### Segment Building Pipelines

The store is populated by multiple pipelines running continuously:

| Pipeline | Schedule | What it builds |
|---|---|---|
| **Behavioral segment builder** | Real-time (NATS consumer) | "clicked_car_ads_7d", "visited_travel_sites" from impression/click events |
| **Retargeting builder** | Real-time (NATS consumer) | "viewed_product_X", "cart_abandoner" from data collection pixels |
| **Predictive segment builder** | Daily (CronJob) | "likely_to_convert", "high_ltv_predicted" from ML models |
| **Lookalike builder** | On-demand (triggered by API) | "lookalike_best_customers_5pct" from seed audiences |
| **CDP sync** | Real-time (webhook) | External CDP segments synced into store |
| **Publisher data ingestion** | Real-time (ad tag) | Publisher-provided attributes normalised into segments |
| **CRM match** | On upload + periodic refresh | Advertiser customer lists matched against identity graph |

#### Audience Store Monitoring

| Metric | What it tracks |
|---|---|
| `audience_store_read_latency_ms` | Read latency (target <2ms p99) |
| `audience_store_segments_per_user` | Average segments per user (memory planning) |
| `audience_store_total_users` | Total unique users in store |
| `audience_store_total_memberships` | Total user-segment memberships |
| `audience_store_write_rate` | Writes per second (ingestion throughput) |
| `audience_store_access_denied_total` | Filtered segments due to access control (security metric) |
| `audience_store_expired_total` | Segments cleaned up due to expiry |
| `audience_store_redis_hit_rate` | Redis hit rate vs Postgres fallback |

#### Audience Store in the Publisher Simulator

The debug overlay shows which segments a simulated user has:

```
Debug Panel - User Segments:
    Platform: sports_fan, clicked_car_ads_7d, likely_to_convert
    Publisher: subscriber, age_25_34 (pub_789 first-party)
    Advertiser: high_value, existing_customer (adv_456 CRM)
    Marketplace: uk_sports (purchased from pub_789)
    Total: 8 segments | Access-filtered for requesting DSP: 6 visible
```

### Implementation

| Component | Location |
|---|---|
| Unified audience store | `pkg/audience/store/` - Redis read/write, access filtering, Postgres durability |
| Ingest normaliser | `pkg/audience/store/ingest.go` - source-specific -> unified format |
| Read with access control | `pkg/audience/store/read.go` - single-lookup with filtering |
| Behavioral segment builder | `pkg/audience/behavioral.go` - NATS consumer, rule-based segment assignment |
| Retargeting builder | `pkg/audience/retargeting.go` - data pixel events -> retargeting segments |
| Predictive builder | `cmd/optimise/` + `python/audience/` - ML scoring -> segment membership |
| Audience segment CRUD | `pkg/audience/segments.go` |
| Lookalike modeling | `pkg/audience/lookalike.go` (Go inference) + `python/audience/` (model training) |
| Composite segment evaluation | `pkg/audience/composite.go` - boolean expression evaluator |
| Audience analytics | `pkg/audience/analytics.go` - HLL size/overlap, composition queries |
| Data collection pixel | Tracker: `GET /v1/t/data` endpoint |
| Pixel management | Gateway API + Ad Server stores pixel configs |
| CDP connectors | `pkg/audience/cdp.go` - webhook receiver, segment sync |
| Taxonomy mapping | `pkg/audience/taxonomy.go` - IAB category tagging |
| Retargeting segment builder | `pkg/audience/retargeting.go` - auto-builds segments from event data |

---

## Clean Rooms and Data Marketplace

### Overview

Data is the most valuable asset in ad tech, but privacy regulations and competitive concerns prevent parties from sharing it directly. Clean rooms solve this: a secure computation environment where multiple parties' data goes in, only aggregate results come out. Nobody sees the other's raw data.

The data marketplace builds on clean rooms: data owners list their audiences, buyers browse and see estimated value before purchasing access, and parties can barter data for mutual benefit.

**This is a platform differentiator.** Most ad tech platforms don't have this built in - advertisers and publishers must use external services (LiveRamp, InfoSum, Snowflake) and then connect the results back. Having it native means zero friction.

### Clean Room Architecture

```
Party A (Advertiser)                Clean Room                    Party B (Publisher)
                                    (isolated computation)
Upload: 500K hashed                                              Upload: 2M hashed
emails + segments         ──>    ┌─────────────────┐    <──     emails + segments
                                 │                 │
                                 │  Compute:       │
                                 │  - Overlap size │
                                 │  - Segment comp │
                                 │  - Expansion    │
                                 │  - Reach est    │
                                 │                 │
                                 └────────┬────────┘
                                          │
                                     Aggregate results only
                                     (no raw data leaves)
                                          │
                              ┌───────────┴───────────┐
                              │                       │
                    Party A sees:              Party B sees:
                    "150K overlap (30%)"       "150K overlap (7.5%)"
                    "1.85M new reachable"      "350K with purchase intent"
                    "Sports segment: 800K"     "High-value segment: 200K"
```

**Key principle: raw data NEVER leaves the clean room.** Only aggregate statistics (counts, percentages, distributions) are returned. Neither party can reverse-engineer individual users from the results.

### How a Clean Room Computation Works

```
1. Party A creates a clean room request:
    POST /v1/api/cleanroom
    {
        "name": "Audience overlap analysis",
        "party_a": {"account_id": "adv_123", "audience_id": "aud_customers"},
        "party_b_invite": {"account_id": "pub_456"},
        "computations": ["overlap", "expansion", "segment_composition"]
    }

2. Party B receives invitation and approves:
    POST /v1/api/cleanroom/{id}/approve
    {
        "audience_id": "aud_all_subscribers",
        "segments_to_share": ["sports", "finance", "tech"]
    }

3. Clean room computation runs (isolated K8s Job):
    a. Load Party A hashed IDs + segments into memory
    b. Load Party B hashed IDs + segments into memory
    c. Compute overlap using set intersection on hashed IDs
    d. Compute expansion: Party B users NOT in Party A
    e. Compute segment composition of overlap
    f. Build HyperLogLog sketches for efficient future queries
    g. Store ONLY aggregate results (counts, percentages)
    h. Destroy raw data in memory (never persisted together)

4. Both parties see results:
    GET /v1/api/cleanroom/{id}/results
    {
        "overlap": {
            "count": 150000,
            "pct_of_party_a": 30.0,
            "pct_of_party_b": 7.5
        },
        "expansion": {
            "party_a_new_reachable": 1850000,
            "party_b_new_signals": 350000
        },
        "segment_composition": {
            "party_b_sports": {"overlap_with_a": 45000, "expansion_for_a": 755000},
            "party_b_finance": {"overlap_with_a": 62000, "expansion_for_a": 438000},
            "party_b_tech": {"overlap_with_a": 38000, "expansion_for_a": 362000}
        }
    }
```

### Privacy Guarantees

| Guarantee | How it's enforced |
|---|---|
| No raw data exposure | Clean room computation runs in isolated K8s Job with no network access except to read input and write aggregate results |
| Minimum aggregation threshold | Results suppressed if any group has < 100 users (prevents identification) |
| No individual-level export | API only returns counts, percentages, distributions - never user lists |
| Hashed IDs only | Both parties upload SHA256-hashed identifiers. Platform never sees raw emails/phones. |
| Audit trail | Every clean room computation is logged: who requested, who approved, what was computed, when |
| Data destruction | Raw matched data destroyed after computation. Only aggregates persist. |
| Consent | Only users who consented to data sharing are included in clean room inputs |
| Opt-out respected | Opted-out users excluded from clean room datasets automatically |

### Data Marketplace

A storefront where data owners list their audience data for others to purchase or barter.

#### How Listings Work

```
Publisher A lists their sports audience:
    POST /v1/api/marketplace/listings
    {
        "name": "UK Sports Enthusiasts",
        "description": "2M+ users who regularly read sports content",
        "audience_id": "aud_sports_pub_a",
        "size_estimate": 2100000,
        "demographics": {"geo": "UK", "age_range": "18-45", "gender_split": "65% male"},
        "pricing": {
            "cpm_surcharge": 0.50,
            "monthly_flat_fee": null,
            "barter_eligible": true
        },
        "preview": {
            "top_iab_categories": ["IAB17-1", "IAB17-12", "IAB17-18"],
            "device_split": {"mobile": 60, "desktop": 35, "tablet": 5},
            "geo_split": {"UK_england": 75, "UK_scotland": 15, "UK_wales": 10}
        }
    }
```

The listing shows aggregate characteristics but NEVER individual users. Buyers browse and assess value before committing.

#### Expansion Estimates (Before Purchase)

A buyer can request an expansion estimate WITHOUT purchasing - this uses a clean room computation:

```
Advertiser X browses marketplace, sees "UK Sports Enthusiasts" listing
    |
    v
Request expansion estimate:
    POST /v1/api/marketplace/listings/{id}/estimate
    {
        "my_audience_id": "aud_my_customers"
    }
    |
    v
Clean room runs lightweight overlap computation:
    Response:
    {
        "listing": "UK Sports Enthusiasts",
        "your_audience_size": 500000,
        "overlap": 85000,
        "overlap_pct": 17.0,
        "new_reachable_users": 2015000,
        "expansion_factor": "4.0x",
        "estimated_value": {
            "at_cpm_surcharge_0.50": "$1,007/month at current impression volume",
            "estimated_incremental_reach": "+2M users you can't reach today"
        }
    }
    |
    v
Advertiser decides: "4x expansion for $0.50 CPM surcharge? Worth it."
```

#### Purchase Flow

```
Advertiser purchases access to listing:
    POST /v1/api/marketplace/listings/{id}/purchase
    {
        "billing": "cpm_surcharge"
    }
    |
    v
Platform creates a data activation link:
    - Advertiser can now TARGET the publisher's segment in their line items
    - Impressions served using this data have the surcharge added
    - Publisher earns the surcharge revenue
    - The segment appears in advertiser's targeting options as "Marketplace: UK Sports Enthusiasts"
    |
    v
Advertiser adds to line item targeting:
    targeting:
      include:
        segments: ["marketplace:uk_sports_enthusiasts"]
    |
    v
During bidding:
    DSP checks: does this user match marketplace:uk_sports_enthusiasts?
    -> Lookup against the data provider's segment (via clean room activation)
    -> Match found: include in bid evaluation
    -> Impression served: CPM surcharge added to clearing price
    -> Surcharge flows to data provider in billing
```

**Key: the advertiser never gets a list of users.** They can target the segment but never export or see individual members. Data stays with the provider.

#### Pricing Models

| Model | How it works | Best for |
|---|---|---|
| **CPM surcharge** | Buyer pays extra $X CPM on impressions using this data | Large-scale targeting, pay as you go |
| **Monthly flat fee** | Fixed monthly fee for unlimited access to the segment | High-volume buyers, predictable cost |
| **Per-query fee** | Pay per clean room computation | Analytics/research use |
| **Barter** | Trade data access instead of paying cash | Mutual benefit, no cash needed |

#### Data Bartering

Two parties exchange data access instead of paying:

```
Publisher A has: "UK Sports Enthusiasts" (2M users)
Advertiser X has: "In-Market Auto Buyers" (300K users)

Both would benefit from the other's data:
    Publisher A wants: auto buyer signals to sell higher-CPM automotive ad inventory
    Advertiser X wants: sports audience to expand their targeting

Barter proposal:
    POST /v1/api/marketplace/barter
    {
        "proposer": "adv_x",
        "proposer_offering": "aud_in_market_auto",
        "counterparty": "pub_a",
        "requesting": "listing_uk_sports",
        "terms": "mutual_access"
    }
    |
    v
Clean room runs expansion estimate for BOTH parties:
    {
        "proposer_gains": {
            "new_reachable": 1800000,
            "expansion_factor": "6x"
        },
        "counterparty_gains": {
            "new_signals": 250000,
            "estimated_revenue_uplift": "$15,000/month from better auto targeting"
        },
        "fairness_score": 0.72
    }
    |
    v
Publisher A reviews: "I get 250K high-intent auto signals for free? Deal."
    POST /v1/api/marketplace/barter/{id}/accept
    |
    v
Both parties now have targeting access to each other's segments
No cash exchanged. Both gain expansion.
```

**Fairness scoring:**

The platform computes a fairness score for barter proposals:

```
fairness_score = min(value_to_a, value_to_b) / max(value_to_a, value_to_b)

1.0 = perfectly balanced trade (both gain equal value)
0.5 = one party gains 2x more than the other
< 0.3 = very unbalanced, platform suggests adding cash to balance
```

Value is estimated from: segment size, segment uniqueness (how much is already reachable), historical CPM of users in the segment, and market demand for the segment's attributes.

#### Marketplace Discovery

Buyers browse the marketplace by:

| Filter | Example |
|---|---|
| Segment category | Sports, Finance, Travel, Auto |
| Geo | UK, US, DE |
| Size | 100K+, 500K+, 1M+ |
| Data type | Publisher behavioural, purchase history, demographic |
| Price range | < $0.50 CPM, < $1.00 CPM |
| Barter eligible | Yes/No |
| Expansion estimate | "Show me segments that would give me 2x+ expansion" |

**Marketplace dashboard:**

```
┌──────────────────────────────────────────────────────────────┐
│  Data Marketplace                                             │
│                                                               │
│  Filter: [Sports ▼] [UK ▼] [500K+ ▼] [Barter OK ☑]         │
│                                                               │
│  ┌─ UK Sports Enthusiasts ──────────────────────────────┐    │
│  │  Publisher: SportMedia Ltd | Size: 2.1M              │    │
│  │  Geo: UK | Age: 18-45 | 65% male                    │    │
│  │  Price: $0.50 CPM surcharge | Barter: eligible       │    │
│  │  Your expansion: 4.0x (+2M users)                    │    │
│  │  [View Details] [Request Estimate] [Purchase] [Barter]│   │
│  └──────────────────────────────────────────────────────┘    │
│                                                               │
│  ┌─ Premium Football Fans ──────────────────────────────┐    │
│  │  Publisher: FootballDaily | Size: 800K               │    │
│  │  Geo: UK, DE, FR | Age: 21-40 | 80% male            │    │
│  │  Price: $0.75 CPM surcharge | Barter: eligible       │    │
│  │  Your expansion: 2.3x (+600K users)                  │    │
│  │  [View Details] [Request Estimate] [Purchase] [Barter]│   │
│  └──────────────────────────────────────────────────────┘    │
│                                                               │
│  My Listings: 2 active | Barter Proposals: 1 pending         │
│  Data Revenue This Month: $4,200                             │
└──────────────────────────────────────────────────────────────┘
```

#### Data Provider Revenue

Data providers (publishers, advertisers listing their data) earn revenue:

| Source | How |
|---|---|
| CPM surcharge | Buyer pays $0.50 CPM extra, data provider receives it (minus platform fee) |
| Monthly fees | Flat fee subscribers, data provider receives it |
| Barter value | No cash but gains targeting access to the other party's data |
| Clean room queries | Per-computation fee for analytics use |

Data revenue tracked separately in billing:
- `GET /v1/api/marketplace/revenue` - data provider's marketplace earnings
- Separate line item on publisher/advertiser payout/invoice
- Platform takes a marketplace fee (configurable, e.g. 15% of data transaction value)

#### Data Governance

| Rule | Enforcement |
|---|---|
| No raw data export | API never returns individual user lists from marketplace segments |
| Activation only | Purchased data can only be used for targeting on our platform, not exported |
| Revocation | Data provider can revoke access at any time. Buyer's targeting using that segment stops immediately. |
| Expiry | Data access expires after agreed period (monthly renewal or fixed term) |
| Usage reporting | Data providers see how their data is being used: impressions targeted, revenue generated, by which buyers |
| Consent propagation | If a user opts out, they're removed from all marketplace segments they're in |
| Minimum listing size | Segments must have > 10,000 users to list (prevents identification risk) |
| Category restrictions | Sensitive data categories (health, political, financial) have additional approval requirements |

#### Clean Room Computation Types

| Computation | What it produces | Use case |
|---|---|---|
| **Overlap analysis** | How many users exist in both datasets | "Do our audiences overlap? By how much?" |
| **Expansion estimate** | How many new users would the buyer gain | "How much bigger would my targeting get?" |
| **Segment composition** | Breakdown of overlap by segments | "Which of your segments overlap most with my customers?" |
| **Lookalike from overlap** | Build a lookalike from the overlapping users | "Find more users like the ones we share" |
| **Attribution study** | Did users exposed to ads on Publisher A convert on Advertiser B's site? | "Did my ads on your site drive sales?" |
| **Reach/frequency across parties** | Unduplicated reach across multiple publishers | "If I buy on Publisher A and B, how many unique users do I reach total?" |
| **Audience enrichment preview** | What additional attributes does Party B's data add to Party A's users? | "What would I learn about my customers from your data?" |

### API Endpoints

**Clean Rooms:**
- `POST   /v1/api/cleanroom` - create clean room and invite counterparty
- `GET    /v1/api/cleanroom` - list my clean rooms (active, pending, completed)
- `GET    /v1/api/cleanroom/{id}` - clean room details and status
- `POST   /v1/api/cleanroom/{id}/approve` - counterparty approves and selects data
- `POST   /v1/api/cleanroom/{id}/reject` - counterparty declines
- `GET    /v1/api/cleanroom/{id}/results` - aggregate results
- `POST   /v1/api/cleanroom/{id}/recompute` - rerun with updated data

**Marketplace:**
- `GET    /v1/api/marketplace/listings` - browse listings (with filters)
- `POST   /v1/api/marketplace/listings` - create a listing (list your data)
- `GET    /v1/api/marketplace/listings/{id}` - listing detail
- `PUT    /v1/api/marketplace/listings/{id}` - update listing
- `DELETE /v1/api/marketplace/listings/{id}` - remove listing
- `POST   /v1/api/marketplace/listings/{id}/estimate` - expansion estimate (triggers clean room)
- `POST   /v1/api/marketplace/listings/{id}/purchase` - purchase access
- `DELETE /v1/api/marketplace/listings/{id}/purchase` - cancel access

**Barter:**
- `POST   /v1/api/marketplace/barter` - propose a barter trade
- `GET    /v1/api/marketplace/barter` - list my barter proposals (sent and received)
- `GET    /v1/api/marketplace/barter/{id}` - proposal detail with fairness score
- `POST   /v1/api/marketplace/barter/{id}/accept` - accept barter
- `POST   /v1/api/marketplace/barter/{id}/reject` - reject barter
- `POST   /v1/api/marketplace/barter/{id}/counter` - counter-propose with different terms

**Data Revenue:**
- `GET    /v1/api/marketplace/revenue` - data provider earnings
- `GET    /v1/api/marketplace/usage` - how your listed data is being used

### gRPC Service

**CleanRoomService (Gateway or dedicated service):**
- `CreateCleanRoom(CleanRoomRequest) -> CleanRoomResponse`
- `ApproveCleanRoom(ApprovalRequest) -> ApprovalResponse`
- `GetResults(CleanRoomID) -> CleanRoomResults`
- `RunComputation(ComputationType, CleanRoomID) -> ComputationResult`

**MarketplaceService (Gateway or dedicated service):**
- `ListListings(MarketplaceFilter) -> ListingList`
- `CreateListing(Listing) -> ListingResponse`
- `EstimateExpansion(ListingID, BuyerAudienceID) -> ExpansionEstimate`
- `PurchaseAccess(ListingID, BillingModel) -> PurchaseResponse`
- `ProposeBarter(BarterProposal) -> BarterResponse`
- `GetFairnessScore(BarterID) -> FairnessScore`

### NATS Subjects

| Subject | Publisher | Subscribers | Payload |
|---|---|---|---|
| `adtech.cleanroom.requested` | Gateway | Clean room job runner | CleanRoomRequest |
| `adtech.cleanroom.completed` | Job runner | Gateway (notifications), Audit | CleanRoomResults |
| `adtech.marketplace.purchased` | Gateway | Billing, Audience activation | PurchaseEvent |
| `adtech.marketplace.barter.accepted` | Gateway | Audience activation (both parties) | BarterAcceptedEvent |
| `adtech.marketplace.revoked` | Data provider | Audience deactivation | RevocationEvent |

### Implementation

| Component | Location |
|---|---|
| Clean room computation engine | `pkg/cleanroom/compute.go` - overlap, expansion, composition, lookalike |
| Clean room isolation | `cmd/cleanroom/` - K8s Job, network-isolated, data destroyed after computation |
| Marketplace listings | `pkg/marketplace/listings.go` - CRUD, search, filtering |
| Expansion estimator | `pkg/marketplace/expansion.go` - lightweight clean room for estimates |
| Barter engine | `pkg/marketplace/barter.go` - proposal, fairness scoring, acceptance |
| Data activation | `pkg/audience/activation.go` - make purchased segments available for targeting |
| Marketplace billing | `pkg/billing/marketplace.go` - CPM surcharge tracking, data provider payouts |
| Fairness scoring | `pkg/marketplace/fairness.go` - value estimation, score calculation |

### For MVP

Clean rooms and marketplace are phased:

1. **Phase 1:** Basic clean room - two-party overlap analysis, expansion estimates
2. **Phase 2:** Marketplace listings - browse, estimate, purchase with CPM surcharge
3. **Phase 3:** Data bartering - proposals, fairness scoring, mutual activation
4. **Phase 4:** Advanced computations - lookalike from overlap, attribution studies, cross-publisher reach

---

## DSP Advanced Features

### Auto-Optimisation

The DSP automatically shifts budget between line items within an IO based on performance, without manual intervention.

```
Insertion Order: "Q3 Brand Campaign" - $50,000 budget
    |
    +-- Line Item A (UK Mobile): spending $200/day, CPA $4.50 (below target $5)
    +-- Line Item B (DE Desktop): spending $150/day, CPA $8.00 (above target $5)
    +-- Line Item C (FR All): spending $100/day, CPA $4.80 (below target $5)
    |
    v
Auto-optimiser runs hourly:
    Line Item A: performing well -> increase daily budget share
    Line Item B: underperforming -> decrease daily budget share
    Line Item C: performing well -> increase daily budget share
    |
    v
Adjusted:
    Line Item A: $250/day (+25%)
    Line Item B: $80/day (-47%)
    Line Item C: $170/day (+70%)
    Total still = $500/day (IO daily budget unchanged)
```

**Controls:**

| Setting | What it does | Default |
|---|---|---|
| `auto_optimise: true/false` | Enable/disable per IO | false |
| `optimise_goal` | What metric to optimise for (CPA, ROAS, CTR, VCR) | Campaign's bid strategy goal |
| `min_budget_share` | Minimum % of IO budget any line item can get | 10% |
| `max_budget_shift_pct` | Maximum % budget can shift per optimisation cycle | 30% |
| `learning_period_hours` | Don't optimise until this much data is collected | 24h |
| `revert_if_worse` | Auto-revert if performance degrades after shift | true |

All shifts are logged in the audit trail and visible in the campaign dashboard.

### Campaign Recommendations

The DSP surfaces actionable recommendations based on campaign performance:

| Recommendation | Trigger | Suggestion |
|---|---|---|
| "Targeting too narrow" | < 1000 impressions/day despite budget available | "Broaden geo from UK_london to UK" |
| "Budget will exhaust early" | Pacing ahead of schedule | "Reduce daily budget or extend flight dates" |
| "Creative fatigue" | CTR declining over 7 days | "Rotate in new creatives" |
| "Frequency too high" | Average frequency > 8 | "Lower frequency cap or broaden audience" |
| "Strong performer" | One line item significantly outperforming | "Increase budget for this line item" |
| "Low viewability" | Viewability rate < 40% | "Exclude low-viewability placements" |
| "Bid too low" | Win rate < 10% | "Increase base bid or adjust modifiers" |
| "Bid too high" | Win rate > 90% with high clearing price gap | "Lower base bid - overpaying" |
| "Audience overlap" | Two line items targeting similar audiences | "Consider merging or differentiating targeting" |
| "Missing conversions" | Clicks but no conversions | "Check conversion pixel is installed correctly" |

**API:**
- `GET /v1/api/campaigns/{id}/recommendations` - recommendations for a line item
- `GET /v1/api/insertion-orders/{id}/recommendations` - IO-level recommendations
- `POST /v1/api/campaigns/{id}/recommendations/{rid}/apply` - one-click apply a recommendation
- `POST /v1/api/campaigns/{id}/recommendations/{rid}/dismiss` - dismiss (don't show again)

Recommendations generated by `cmd/optimise/` pipeline, stored in Postgres, surfaced in dashboard with "Apply" and "Dismiss" buttons.

### Multi-Arm Bandit Creative Rotation

Beyond simple A/B testing, use a Thompson Sampling multi-arm bandit for creative rotation:

```
Line item has 4 creatives:
    Creative A: 1000 impressions, 12 clicks (CTR 1.2%)
    Creative B: 1000 impressions, 18 clicks (CTR 1.8%)
    Creative C: 500 impressions, 3 clicks (CTR 0.6%)
    Creative D: 200 impressions, 5 clicks (CTR 2.5%) <- small sample

Standard A/B: equal rotation, wait for statistical significance (slow)

Multi-arm bandit: dynamically allocate more traffic to better performers
    while still exploring underexplored options (Creative D)

    Rotation this hour:
    Creative A: 20% of impressions (decent, but not best)
    Creative B: 35% (strong performer)
    Creative C: 5% (poor performer, minimal exploration)
    Creative D: 40% (promising but uncertain - explore more)
```

Balances **exploitation** (show what works) with **exploration** (test what might work better). Converges to the best creative faster than traditional A/B with less wasted spend on poor performers.

Implemented in `pkg/adserving/bandit.go`. Runs at the Ad Server during creative selection.

### Algorithmic Bidding

Beyond rule-based bid modifiers, ML-based bid prediction:

```
Traditional:
    base_bid * device_modifier * geo_modifier * time_modifier * shading
    = deterministic, same bid for same inputs

Algorithmic:
    ML model predicts: P(conversion | this impression context)
    bid = P(conversion) * target_CPA * campaign_value_factor
    = adaptive, learns from outcomes
```

**How it works:**

| Step | What happens |
|---|---|
| 1. Feature extraction | Extract features from bid request: user segments, placement, geo, device, time, publisher, content |
| 2. Prediction | ML model predicts probability of desired outcome (click, conversion, completion) |
| 3. Valuation | bid = P(outcome) * value_per_outcome (from campaign CPA/ROAS target) |
| 4. Shading | Apply bid shading to the predicted-value bid |
| 5. Modifiers | Apply manual bid modifiers on top (advertiser overrides) |
| 6. Submit | Final bid sent to exchange |

Model trained offline in Python (`python/bidding/`), exported as ONNX or simple feature weights consumed by Go in real-time. Retrained daily on previous day's outcome data.

For MVP: rule-based bidding (modifiers + shading). Algorithmic bidding added in optimisation phase.

### DSP Implementation

| Component | Location |
|---|---|
| Auto-optimiser | `cmd/optimise/` - hourly budget reallocation across line items |
| Recommendations engine | `cmd/optimise/` - generates recommendations from performance data |
| Recommendations API | Gateway: `/v1/api/campaigns/{id}/recommendations` |
| Multi-arm bandit | `pkg/adserving/bandit.go` - Thompson Sampling creative rotation |
| Algorithmic bidding | `pkg/auction/ml_bidder.go` (Go inference) + `python/bidding/` (model training) |

---

## SSP Advanced Features

### Yield Analytics Dashboard

Publishers need deep insight into how their inventory performs and how to maximise revenue:

| Metric | What it shows | Why it matters |
|---|---|---|
| Fill rate | % of ad requests that resulted in a served ad | Low fill = wasted inventory |
| eCPM | Effective CPM across all billing models | True revenue per 1000 impressions |
| Revenue by placement | Which placements earn most | Focus optimization on top placements |
| Revenue by hour | How revenue varies by time of day | Set time-based floor prices |
| Revenue by device | Mobile vs desktop earnings | Understand audience device mix |
| Bid density | Average number of bids per auction | Low density = need more demand |
| Win rate by DSP | Which DSPs win most often | Understand demand composition |
| Floor price analysis | Current floor vs clearing prices | Floor too high? Losing fill. Too low? Leaving money on table. |
| Viewability by placement | Which placements have high/low viewability | Low viewability = lower bids, fix placement position |
| Latency by DSP | Which DSPs are slowest to respond | Slow DSPs reduce auction competition |

**Yield recommendations:**

| Recommendation | Trigger | Suggestion |
|---|---|---|
| "Floor too high" | Fill rate < 50%, most losing bids are above 0 but below floor | "Lower floor from $2.00 to $1.50 - estimated +30% fill rate, +15% revenue" |
| "Floor too low" | Clearing price consistently 3x above floor | "Raise floor from $0.50 to $1.50 - capture more value without losing fill" |
| "Low viewability" | Viewability < 40% on a placement | "Move ad slot above the fold or increase ad size" |
| "Underperforming placement" | eCPM < $0.50 | "Consider removing this placement or improving its position" |
| "Missing demand" | Bid density < 2 for a category | "Not enough advertisers targeting this category - consider PMP deals" |

### Dynamic Floor Pricing (ML-Based)

Instead of static floor prices, ML predicts the optimal floor for each impression:

```
Bid request arrives for placement X:
    Time: 14:30, Device: mobile, Geo: UK, Content: sports
    |
    v
Floor price model predicts:
    Historical data: this combination clears at $1.80 avg, $2.50 p75
    Current demand: 8 DSPs active for this targeting (high competition)
    Predicted optimal floor: $1.60
    |
    v
Compare with:
    Publisher's static floor: $1.00
    Model's dynamic floor: $1.60
    Applied floor: max(static, dynamic) = $1.60
```

The model optimises for **total revenue** (not just CPM), balancing higher floors (higher price per impression) against lower fill rate (fewer impressions sold).

Trained on: historical clearing prices, bid density, time of day, geo, device, content category, seasonal trends. Updated daily by `cmd/optimise/`.

### Header Bidding Design (Deferred but Architected)

Header bidding allows publishers to simultaneously solicit bids from multiple exchanges/SSPs before making their ad server call. Our platform can participate as either:

**As a demand source (our DSP bids via another SSP's header bidding):**

```
Publisher page loads
    |
    v
Prebid.js in browser calls multiple SSPs simultaneously:
    SSP A: calls our platform for bids
    SSP B: calls competitor platform
    SSP C: calls another competitor
    |
    v
Each SSP returns bids
    |
    v
Prebid.js picks the highest bid, passes to publisher's ad server
    |
    v
If our bid won: our creative served
```

**As a server-side bidding endpoint (Prebid Server compatible):**

```
Publisher's server calls our Prebid Server adapter:
    POST /v1/openrtb/prebid
    Body: standard OpenRTB bid request
    |
    v
Our Exchange runs auction, returns bid
    |
    v
Publisher's server collects bids from all sources, picks winner
```

Integration point: `pkg/openrtb/prebid.go` - adapter that translates between Prebid Server protocol and our internal auction.

### Unified Auction (Multiple Demand Sources)

For publishers using our SSP, we can run a unified auction that combines:

```
Bid request from publisher placement
    |
    v
Our SSP simultaneously solicits bids from:
    +-- Our own DSP (internal gRPC)
    +-- External DSP A (OpenRTB HTTP)
    +-- External DSP B (OpenRTB HTTP)
    +-- Google AdX adapter (OpenRTB HTTP)
    |
    v
All bids collected (with timeout)
    |
    v
Our Exchange runs unified auction across all demand sources
    |
    v
Winner served (internal or external)
```

This is the full-featured SSP model: our publishers get access to demand from everywhere, not just our own DSP. Requires:
- External DSP onboarding (API key, endpoint URL, timeout config)
- Demand source management UI for publishers
- Per-demand-source reporting (which sources provide best value)

### Real-Time Ad Quality Scanning

Beyond creative review at upload, scan ads at serve time:

```
Exchange selects winning bid
    |
    v
Ad quality scanner checks the creative URL:
    1. Is the landing URL still reachable? (not 404)
    2. Does the landing URL match the declared domain?
    3. Is the creative loading malware? (check against malware URL lists)
    4. Has this creative been flagged by users/publishers recently?
    |
    v
Pass -> serve the ad
Fail -> serve next-best bid, flag creative for review
```

Runs asynchronously - the first serve goes through, scanner checks in background. If flagged, subsequent serves are blocked until reviewed.

### Publisher SDK Documentation

The `adtech.js` SDK needs comprehensive documentation for publishers:

| Doc section | What it covers |
|---|---|
| Quick start | Minimal integration: one script tag + one ad slot |
| Ad slot configuration | Sizes, formats, floor prices, quality controls |
| User data passing | How to send first-party data (segments, demographics) |
| Consent integration | How to pass CMP consent signals |
| Video integration | HLS.js player + VAST/VMAP setup |
| Native rendering | Template setup for native ads |
| Event callbacks | JavaScript hooks for impression, click, viewability |
| Debugging | Enable debug mode, view trace IDs in console |
| A/B testing | Test different ad configurations |

Documentation auto-generated from code comments + handwritten guides. Hosted at `/docs/sdk/`.

### SSP Implementation

| Component | Location |
|---|---|
| Yield analytics | `pkg/reporting/yield.go` - publisher yield metrics and recommendations |
| Dynamic floor pricing | `pkg/targeting/floors.go` (Go inference) + `python/floors/` (model training) |
| Header bidding adapter | `pkg/openrtb/prebid.go` - Prebid Server compatible endpoint |
| Unified auction | `cmd/exchange/` - external demand source management |
| Ad quality scanner | `pkg/adserving/scanner.go` - real-time URL and creative checking |
| Publisher SDK docs | `docs/sdk/` - auto-generated + handwritten integration guides |
| External DSP management | SSP: `pkg/demand/sources.go` - register, configure, monitor external demand |

---

## Developer Tools

### Trace Explorer

A visual tool for tracing a single ad request through the entire system in real time. Two interfaces: a Grafana dashboard for deep analysis, and a custom HTMX page for the live animated view.

#### Grafana Trace Dashboard

Pre-built dashboard that takes a `trace_id` as a variable. Zero custom code needed - built on existing infrastructure:

| Panel | Source | Shows |
|---|---|---|
| Span waterfall | Jaeger | Timing of each service hop as a waterfall diagram |
| Log timeline | Loki (`{trace_id="abc"}`) | Every log line across all services, ordered by time |
| Event records | Analytics store | Impression, click, conversion, viewability events for this trace |
| Auction detail | Analytics store | Bids received, winner, clearing price, loss reasons |
| Budget impact | Postgres/Redis | Budget before/after, reservation/settlement |

#### Live Trace Explorer (Custom HTMX Page)

A real-time animated view of a request flowing through the system. Available at `/dev/trace-explorer`.

```
Developer opens trace explorer
    |
    v
Enters trace_id (or clicks "Fire single request")
    |
    v
SSE (Server-Sent Events) connection opens
    Gateway tails Loki for this trace_id in real time
    |
    v
As each service processes the request, nodes light up on the flow diagram:

  t=0ms    [SSP] ●── "bid request created"
  t=2ms    [Exchange] ●── "auction started, 3 DSPs eligible"
  t=5ms    [DSP 1] ●── "evaluating: campaign X matches, bid $2.50"
  t=8ms    [DSP 2] ●── "evaluating: campaign Y matches, bid $3.00"
  t=12ms   [DSP 3] ○── "no-bid: budget depleted"
  t=15ms   [Exchange] ●── "auction complete: DSP 2 wins at $3.00 (first-price)"
  t=16ms   [DSP 1] ○── "loss notice: outbid, clearing $3.00"
  t=18ms   [Ad Server] ●── "serving creative cr_456, 3 macros substituted, 2 third-party pixels"
  t=20ms   [Browser] ●── "ad rendered"
  t=1200ms [Tracker] ●── "impression pixel received, fraud_score: 0.05"
  t=2300ms [Tracker] ●── "viewability beacon: 2.3s visible, 85% area"
  t=1201ms [NATS] ●── "ImpressionEvent published to adtech.events.impression"
  t=1205ms [Reporting] ●── "event written to analytics store"
  t=1206ms [Billing] ●── "$3.00 accrued (CPM, immediate billing)"
  t=1207ms [DSP] ●── "budget decremented: $997.00 remaining"
```

Each node is clickable - expands to show the full log entry, timing, request/response data.

**Powered by:**
- Loki live tail API (streams new log lines matching a label filter)
- Gateway converts Loki events to SSE stream
- HTMX `hx-ext="sse"` receives events and swaps in new nodes
- Lightweight inline JS for the animated flow diagram (no framework)

**Additional features:**

| Feature | How it works |
|---|---|
| Side-by-side comparison | Enter two trace_ids, see both flows in parallel - spot where they diverged |
| Replay mode | Load a historical trace_id, replay the flow at real speed or slowed down |
| Anomaly highlighting | Hops that took unusually long show red, missing services show as gaps |
| Budget impact panel | Shows budget before/after, pacing status, bid shading decision |
| Data lineage | Follow the event through rollups - "this impression is in the hourly rollup for 14:00 and the daily rollup for May 27" |

### Publisher Simulator

A simulated publisher website where developers can see the entire ad experience from the end user's perspective. Available at `/dev/publisher-simulator`.

#### What it looks like

A fake news/media website with multiple ad placements:

```
┌──────────────────────────────────────────────────────────────┐
│  The Daily Sim - Your Simulated News Source                   │
│──────────────────────────────────────────────────────────────│
│                                                               │
│  ┌─────────────────── 728x90 HEADER AD ──────────────────┐  │
│  │  [Real ad creative loads here via adtech.js]           │  │
│  │  trace_id: abc-123 (clickable)                         │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                               │
│  Breaking: Local Team Wins    │  ┌── 300x250 SIDEBAR ──┐    │
│  Championship                  │  │ [Real ad loads]      │    │
│                                │  │ trace_id: def-456    │    │
│  Lorem ipsum dolor sit amet,   │  └──────────────────────┘    │
│  consectetur adipiscing elit.  │                              │
│  Sed do eiusmod tempor...      │  ┌── NATIVE AD ────────┐    │
│                                │  │ [Native ad renders   │    │
│  More article content here...  │  │  in publisher style] │    │
│                                │  │ trace_id: ghi-789    │    │
│                                │  └──────────────────────┘    │
│                                │                              │
│  ┌─────────────────── 300x250 FOOTER AD ─────────────────┐  │
│  │  [Real ad loads here]                                  │  │
│  │  trace_id: jkl-012                                     │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
```

Every ad placement runs the real `adtech.js` tag. Real auctions happen. Real creatives are served.

#### Debug Overlay

A floating panel on the page shows everything happening under the hood:

```
┌─── Ad Debug Panel ────────────────────────────────────────┐
│                                                            │
│  Header Ad (728x90) - trace: abc-123                       │
│  ├─ Bid request sent          ✓  0ms                       │
│  ├─ Auction completed         ✓  15ms  (3 bids, winner: $3.00) │
│  ├─ Creative served           ✓  18ms  (cr_456, 300x250)  │
│  ├─ Impression pixel          ✓  22ms  (HTTP 200)         │
│  ├─ Viewability beacon        ✓  1.2s  (85% visible, 1.2s)│
│  ├─ Third-party: DoubleVerify ✓  25ms  (HTTP 200)         │
│  ├─ Third-party: Nielsen      ✗  timeout after 2s          │
│  └─ [View full trace →]                                    │
│                                                            │
│  Sidebar Ad (300x250) - trace: def-456                     │
│  ├─ Bid request sent          ✓  0ms                       │
│  ├─ Auction completed         ✓  12ms  (2 bids, winner: $2.10) │
│  ├─ Creative served           ✓  14ms  (cr_789, 300x250)  │
│  ├─ Impression pixel          ✓  18ms                      │
│  ├─ Viewability beacon        ⏳ waiting (ad below fold)    │
│  └─ [View full trace →]                                    │
│                                                            │
│  Native Ad - trace: ghi-789                                │
│  ├─ Bid request sent          ✓  0ms                       │
│  ├─ Auction completed         ✓  18ms  (native response)  │
│  ├─ Native rendered           ✓  22ms  (title, image, CTA) │
│  ├─ Impression pixel          ✓  25ms                      │
│  └─ [View full trace →]                                    │
│                                                            │
│  Footer Ad (300x250) - trace: jkl-012                      │
│  ├─ Bid request sent          ✓  0ms                       │
│  ├─ Auction completed         ✓  8ms   (no bids - no fill) │
│  ├─ Default ad served         ✓  10ms  (fallback creative) │
│  └─ [View full trace →]                                    │
│                                                            │
│  Page Summary:                                             │
│  Competitive separation: Coca-Cola (header) blocked        │
│    Pepsi from sidebar auction                              │
│  Total page load impact: 45ms for ad rendering             │
│  Consent status: GDPR=true, consent=given                  │
│                                                            │
│  [Fire new page load]  [Change user profile]  [Toggle consent] │
└────────────────────────────────────────────────────────────┘
```

#### Interactive Controls

| Control | What it does |
|---|---|
| **Fire new page load** | Reload the page - new auctions, new ads, new trace IDs |
| **Change user profile** | Switch simulated user: new user (no history), returning user (frequency caps apply), opted-out user (contextual only) |
| **Toggle consent** | Switch between: full consent, no consent (GDPR), COPPA mode - see how ads change |
| **Change geo** | Simulate different countries - see how targeting, floor prices, and currency change |
| **Change device** | Switch between desktop/mobile/tablet - see responsive ads and device targeting |
| **Slow motion** | Add artificial delay between steps so you can watch the flow in slow motion |
| **Click an ad** | Fires the real click tracker, shows the redirect chain in the debug panel, opens landing page |
| **Scroll down** | Viewability beacons fire as ads enter the viewport - debug panel updates in real time |

#### Simulated Publisher Configs (All Channels)

Every ad channel has its own simulator template with channel-specific debug overlay. Each template runs **real auctions** and fires **real events** - the developer sees exactly what an end user would see, plus the debug overlay showing every event.

| Template | Channel | What it simulates | Events traced |
|---|---|---|---|
| `news_site` | Display + Native | News article with header banner, sidebar, in-content native ad, footer banner | Impression pixels, viewability beacons, click redirects, third-party pixels |
| `video_page` | Video (VAST) | Page with HLS.js video player. Pre-roll, mid-roll, post-roll via VMAP. Companion banner. | VAST request, video start/quartiles/complete/skip, companion impression |
| `live_stream` | Video (SSAI) | Live stream player via SSAI manifest. Ads stitched into stream. | Session start, manifest requests, server-side beacons (start/quartiles/complete), session end |
| `ctv_player` | CTV | Simulated CTV player (full-screen video, no click, household targeting) | SSAI beacons, household audience signals, completion rate |
| `podcast` | Audio | Podcast episode player with audio waveform. DAAST pre-roll + mid-roll. Companion banner. | Audio start/quartiles/complete, companion impression, download vs stream tracking |
| `radio_stream` | Audio (Live) | Live radio stream with SSAI-stitched audio ads | Audio session, server-side beacons, stream continuity |
| `billboard` | DOOH | Digital billboard rotation simulator. Configurable venue, weather, time. | Proof-of-play events, audience estimation, rotation timing |
| `retail_search` | Retail | Retailer search results with sponsored products + organic results. Relevance scores shown. | Sponsored product impressions, clicks, CPC billing, conversion (purchase) |
| `game_scene` | In-Game | Game scene with billboards + rewarded ad prompt + interstitial loading screen | Intrinsic viewability (camera angle), rewarded completion, reward verification callback |
| `ecommerce` | Display + Native | Product listing page with product-adjacent display and native ads | Impression, viewability, click |
| `mobile_app` | Mobile Display | Mobile app layout with banner, interstitial, and native ads. IDFA/GAID handling. | App impression, interstitial display/close, native render |
| `minimal` | Display | Single 300x250 ad slot - simplest possible test page | Impression pixel only |
| `multi_format` | All display | Page with every display format: banner, native, video outstream, audio companion | All event types on one page |

**Each template has a channel-specific debug overlay:**

| Channel | Debug overlay shows |
|---|---|
| **Display** | Impression pixel ✓/✗, viewability beacon (duration, % visible), click redirect chain, third-party pixel status |
| **Native** | Asset rendering (title, image, CTA), impression tracker, click tracker |
| **Video (VAST)** | VAST XML request/response, quartile progression bar (start -> Q1 -> mid -> Q3 -> complete), skip event, companion load |
| **Video (SSAI)** | Manifest URL, session ID, segment requests, server-side beacon status, break position, ad pod composition |
| **CTV** | Household ID, device type, session state, completion rate, no-click environment |
| **Audio** | DAAST request/response, listen-through progression bar, download vs stream mode, companion status |
| **DOOH** | Screen ID, rotation slot, play duration, estimated audience count, weather/time signals, proof-of-play confirmation |
| **Retail** | Search query, relevance scores per product, position, CPC, organic vs sponsored ratio |
| **In-Game** | Billboard camera angle/distance/viewability, rewarded opt-in rate, reward verification status, session duration |

**Interactive controls per channel:**

| Control | Display | Video | Audio | DOOH | Retail | In-Game |
|---|---|---|---|---|---|---|
| Fire new request | ✓ | ✓ (new break) | ✓ (next ad) | ✓ (next rotation) | ✓ (new search) | ✓ (rotate billboards) |
| Change user | ✓ | ✓ | ✓ | N/A | ✓ (shopper profile) | ✓ (player profile) |
| Toggle consent | ✓ | ✓ | ✓ | N/A | ✓ | ✓ |
| Change geo | ✓ | ✓ | ✓ | ✓ (change venue location) | ✓ | ✓ |
| Change device | ✓ | ✓ (web/CTV) | ✓ (phone/speaker) | N/A | ✓ (mobile/desktop) | ✓ (phone/tablet) |
| Slow motion | ✓ | ✓ | ✓ | ✓ (slow rotation) | ✓ | ✓ |
| Scroll (viewability) | ✓ | N/A | N/A | N/A | ✓ | N/A |
| Skip ad | N/A | ✓ | ✓ | N/A | N/A | N/A |
| Complete ad | N/A | ✓ (wait/fast-forward) | ✓ | N/A | N/A | ✓ (rewarded) |
| Click/tap ad | ✓ | ✓ | N/A | ✓ (QR scan) | ✓ (product click) | N/A |
| Change weather | N/A | N/A | N/A | ✓ | N/A | N/A |
| Change search query | N/A | N/A | N/A | N/A | ✓ | N/A |
| Trigger rewarded | N/A | N/A | N/A | N/A | N/A | ✓ |
| Change game genre | N/A | N/A | N/A | N/A | N/A | ✓ |

**All templates share the same "View full trace →" link** that opens the Trace Explorer for that specific request.

#### How It Connects to the Trace Explorer

Every ad on the simulated page has a "View full trace →" link. Clicking it opens the Trace Explorer for that specific trace_id. The developer goes from "I can see the ad on the page" to "I can see every system that touched this request" in one click.

```
Publisher Simulator                    Trace Explorer
┌──────────────────┐                  ┌──────────────────────┐
│ [Ad renders]     │    click         │ [SSP] → [Exchange]   │
│ trace: abc-123   │ ──────────────>  │ → [DSP] → [AdServer] │
│ [View trace →]   │                  │ → [Tracker] → [NATS]  │
└──────────────────┘                  │ → [Reporting]         │
                                      └──────────────────────┘
```

#### Implementation

| Component | Location |
|---|---|
| Trace Explorer page | Gateway: `/dev/trace-explorer` - HTMX + SSE |
| Trace SSE endpoint | Gateway: `GET /dev/trace-explorer/{trace_id}/stream` - tails Loki, streams events |
| Publisher Simulator page | Gateway: `/dev/publisher-simulator` - HTMX page with real adtech.js tags |
| Debug overlay JS | `web/static/debug-overlay.js` - intercepts all ad events, shows debug panel |
| Publisher page templates | `web/templates/simulator/` - news_site.html, ecommerce.html, etc. |
| Grafana trace dashboard | `k8s/base/grafana/` - pre-built dashboard JSON |
| Simulator config | `profiles/simulation/` - user profiles, geo presets, consent presets |

**Note:** Developer tools are only available in local and staging environments. The `/dev/*` routes are excluded from production via Kustomize overlay (no Traefik route to them in prod).

---

## Gateway (Single API Entry Point)

### Role

The Gateway is the single entry point for all external traffic. Every external request - dashboard UI, REST API, webhook - goes through the Gateway. Internal services are never exposed directly.

| Responsibility | How |
|---|---|
| All external HTTP traffic | Single K8s ingress point |
| Authentication | JWT issuance, validation, refresh |
| Authorisation | RBAC check before proxying to internal services |
| Tenant context | Extracts account_id from JWT, injects into gRPC metadata |
| API versioning | `/v1/api/...` path prefix |
| Rate limiting | Per-account, per-API-key (Redis-backed) |
| HTMX dashboard | Serves Go HTML templates for the UI |
| Swagger docs | Serves OpenAPI UI at `/docs` |
| Proxying | Translates HTTP/JSON to gRPC calls to internal services |
| Webhook dispatch | Sends outbound notifications to registered URLs |

### Signup and Onboarding

#### Registration

```
User visits platform
    |
    v
Register (email, password, account type: advertiser or publisher)
    |
    v
Email verification (link sent, must verify before proceeding)
    |
    v
Onboarding wizard (account-type specific)
```

#### Advertiser Onboarding

| Step | What they do | Required before |
|---|---|---|
| 1. Company details | Name, address, billing contact | Anything else |
| 2. Billing setup | Pre-pay credit or request credit terms | Creating campaigns |
| 3. Create first campaign | Guided wizard: name, budget, dates, bid strategy | Launching ads |
| 4. Upload first creative | Image, HTML, or native ad content | Launching ads |
| 5. Set targeting | Geo, device, audience segments | Launching ads |
| 6. Launch | Campaign submitted for review, then goes live | - |

#### Publisher Onboarding

| Step | What they do | Required before |
|---|---|---|
| 1. Company details | Name, address, payout contact | Anything else |
| 2. Payout setup | Bank details or payment method | Receiving payouts |
| 3. Register first site/app | Domain, category, description | Creating placements |
| 4. Create first placement | Ad slot: size, format, floor price, page location | Serving ads |
| 5. Get ad tag | JS snippet or tag to paste on their site | Integration |
| 6. Verify integration | Platform fires a test ad request and confirms the tag is working | Going live |

### Webhooks

Advertisers and publishers register webhook URLs to receive notifications when events happen.

| Event | Who receives | Use case |
|---|---|---|
| `budget.depleted` | Advertiser | Top up or pause campaigns |
| `campaign.ended` | Advertiser | Review performance, renew |
| `campaign.approved` | Advertiser | Campaign passed review, now live |
| `invoice.generated` | Advertiser | Payment due |
| `payout.processed` | Publisher | Money is coming |
| `fraud.alert` | Both | Unusual traffic detected on their account |
| `schema.drift` | Publisher | Their data format changed, needs review |
| `report.ready` | Both | Scheduled report is available for download |

Webhook delivery is reliable:
- Events published to NATS subject `adtech.webhooks.{event_type}`
- A webhook dispatcher service consumes from NATS, sends HTTP POST to registered URLs
- Retries with exponential backoff on failure (3 attempts)
- Delivery history visible in dashboard per webhook
- HMAC signature on every payload so receivers can verify authenticity

---

## API Contracts

Contracts fall into four categories: gRPC (internal), OpenRTB JSON/HTTP (bidding), HTTP/JSON (frontend), and NATS subjects (async events). gRPC contracts are enforced by proto definitions. The rest are documented here.

### gRPC Services (Internal)

Defined in `pkg/proto/`. The proto files are the contract - compiler enforces them.

**AuctionService (Exchange)**
- `RunAuction(AuctionRequest) -> AuctionResponse` - SSP calls this to initiate an auction for a placement

**AdService (Ad Server)**
- `ServAd(ServeAdRequest) -> ServeAdResponse` - Exchange calls this with the winning bid to get the creative and tracking URLs
- `GetCreative(CreativeRequest) -> CreativeResponse` - Fetch a creative by ID

**TrackerService (Tracker)**
- `RecordEvent(EventRequest) -> EventResponse` - Internal services push events (impressions, wins, errors)

**CampaignService (DSP)**
- `CreateCampaign(Campaign) -> CampaignResponse`
- `GetCampaign(CampaignID) -> Campaign`
- `GetCampaigns(CampaignFilter) -> CampaignList`
- `UpdateCampaign(CampaignUpdate) -> CampaignResponse`
- `DeleteCampaign(CampaignID) -> DeleteResponse`
- `TransitionState(CampaignID, State) -> CampaignResponse` - lifecycle state changes
- `UpdateBudget(BudgetUpdate) -> BudgetResponse` - budget reservation/settlement
- `GetTargeting(CampaignID) -> TargetingRules`
- `UpdateTargeting(CampaignID, TargetingRules) -> TargetingResponse`
- `GetSchedule(CampaignID) -> Schedule`
- `UpdateSchedule(CampaignID, Schedule) -> ScheduleResponse`
- `AttachCreative(CampaignID, CreativeID) -> LinkResponse`
- `DetachCreative(CampaignID, CreativeID) -> LinkResponse`
- `SetCreativeWeight(CampaignID, CreativeID, Weight) -> WeightResponse`
- `GetTargetingOptions() -> TargetingOptions` - available geos, devices, segments

**InsertionOrderService (DSP)**
- `CreateIO(InsertionOrder) -> IOResponse`
- `GetIO(IOID) -> InsertionOrder`
- `ListIOs(IOFilter) -> IOList`
- `UpdateIO(IOID, IOUpdate) -> IOResponse`
- `DeleteIO(IOID) -> DeleteResponse`
- `GetIOLineItems(IOID) -> LineItemList` - list line items under an IO
- `GetIOBudgetStatus(IOID) -> BudgetStatus` - total budget, spent, remaining, pacing

**AudienceService (DSP)**
- `UploadAudience(AudienceUpload) -> AudienceResponse`
- `ListAudiences(AudienceFilter) -> AudienceList`
- `CreateSegment(SegmentRules) -> SegmentResponse`
- `GetSegment(SegmentID) -> Segment`
- `UpdateSegment(SegmentID, SegmentUpdate) -> SegmentResponse`
- `DeleteSegment(SegmentID) -> DeleteResponse`
- `GetSegmentSize(SegmentID) -> SizeResponse`

**AdService (Ad Server)**
- `ServeAd(ServeAdRequest) -> ServeAdResponse` - Exchange calls with winning bid
- `GetCreative(CreativeID) -> Creative`
- `ListCreatives(CreativeFilter) -> CreativeList`
- `UploadCreative(CreativeUpload) -> CreativeResponse`
- `UpdateCreative(CreativeID, CreativeUpdate) -> CreativeResponse`
- `DeleteCreative(CreativeID) -> DeleteResponse`
- `UpdateCreativeStatus(CreativeID, Status, Reason) -> CreativeResponse` - review workflow

**InventoryService (SSP)**
- `RegisterPublisher(Publisher) -> PublisherResponse`
- `GetPublisher(PublisherID) -> Publisher`
- `ListPublishers(PublisherFilter) -> PublisherList`
- `UpdatePublisher(PublisherID, PublisherUpdate) -> PublisherResponse`
- `DeletePublisher(PublisherID) -> DeleteResponse`
- `RegisterPlacement(Placement) -> PlacementResponse`
- `GetPlacement(PlacementID) -> Placement`
- `GetPlacements(PlacementFilter) -> PlacementList`
- `UpdatePlacement(PlacementID, PlacementUpdate) -> PlacementResponse`
- `DeletePlacement(PlacementID) -> DeleteResponse`
- `SetFloorPrices(PlacementID, FloorPriceConfig) -> FloorPriceResponse`
- `GetFloorRecommendations(PlacementID) -> FloorRecommendations`
- `GetForecast(PlacementID, Days) -> ForecastResponse`
- `GetPlacementHealth(PlacementID) -> HealthResponse`
- `GenerateAdTag(PlacementID) -> AdTagResponse`

**QualityControlService (SSP)**
- `ListQualityControls(PublisherID) -> QualityControlList`
- `AddQualityControl(PublisherID, QualityControl) -> QualityControlResponse`
- `UpdateQualityControl(QualityControlID, QualityControl) -> QualityControlResponse`
- `DeleteQualityControl(QualityControlID) -> DeleteResponse`
- `SyncQualityControls(PublisherID, QualityControlList) -> SyncResponse` - bulk sync

**DealService (SSP)**
- `CreateDeal(Deal) -> DealResponse`
- `GetDeal(DealID) -> Deal`
- `ListDeals(DealFilter) -> DealList`
- `UpdateDeal(DealID, DealUpdate) -> DealResponse`
- `DeleteDeal(DealID) -> DeleteResponse`
- `GetDealPerformance(DealID) -> DealPerformance`

**ReportingService (Reporting)**
- `QueryMetrics(MetricsRequest) -> MetricsResponse`
- `GetTraceEvents(TraceID) -> TraceEventList`
- `RunCustomReport(ReportQuery) -> ReportResult`
- `GetBuilderOptions() -> BuilderOptions` - available metrics, dimensions, filters
- `SaveReport(SavedReport) -> SavedReportResponse`
- `ListSavedReports(AccountID) -> SavedReportList`
- `UpdateSavedReport(SavedReport) -> SavedReportResponse`
- `DeleteSavedReport(ReportID) -> DeleteResponse`

**AuditService (Gateway)**
- `QueryAuditLog(AuditFilter) -> AuditEventList`
- `GetResourceHistory(ResourceType, ResourceID) -> AuditEventList`
- `GetUserActivity(UserID) -> AuditEventList`
- `ExportAuditLog(ExportFilter) -> ExportResponse`

**IdentityService (Gateway)**
- `GetIdentityGraph(UserID) -> IdentityGraph` - platform admin only
- `OptOut(UserID) -> OptOutResponse`
- `RequestDeletion(UserID) -> DeletionResponse`

**AccountService (Gateway)**
- `CreateAccount(Account) -> AccountResponse`
- `Authenticate(Credentials) -> Token`
- `Authorize(Token, Permission) -> AuthResult`
- `GetOnboardingStatus(AccountID) -> OnboardingStatus`
- `CompleteOnboardingStep(AccountID, Step) -> OnboardingStatus`

**BillingService (hosted by Reporting service)**
- `GetBalance(AccountID) -> BalanceResponse` - Current advertiser balance
- `GetInvoices(AccountID, DateRange) -> InvoiceList` - List invoices
- `GetInvoiceDetail(InvoiceID) -> InvoiceDetail` - Invoice with line-item breakdown
- `GetPayouts(AccountID, DateRange) -> PayoutList` - List publisher payouts
- `CreateAdjustment(Adjustment) -> AdjustmentResponse` - Manual credit/debit
- `GetReconciliation(Date) -> ReconciliationReport` - Daily reconciliation results

**FraudService (Fraud)**
- `GetFraudScore(TraceID) -> FraudScoreResponse` - Fraud score for a specific event
- `GetTrafficQuality(AccountID, DateRange) -> TrafficQualityReport` - Traffic quality overview per publisher
- `UpdateBlocklist(BlocklistUpdate) -> BlocklistResponse` - Add/remove IPs, UAs, domains
- `GetBlocklists() -> BlocklistResponse` - Current blocklists
- `GetFlaggedEvents(Filter) -> FlaggedEventList` - Events above fraud threshold

**PipelineService (Pipeline)**
- `IngestFile(FileUpload) -> IngestResponse` - Accept a publisher data file for processing
- `GetFileStatus(FileID) -> FileStatusResponse` - Processing status of an uploaded file
- `GetQuarantine(Filter) -> QuarantineList` - Quarantined files/rows with error details
- `GetDriftReport(PublisherID) -> DriftReport` - Schema drift history for a publisher
- `ApproveDriftMapping(DriftMappingID) -> ApproveResponse` - Confirm a suggested field mapping

**WebhookService (Webhooks)**
- `RegisterWebhook(Webhook) -> WebhookResponse` - Register a webhook URL + subscribed events
- `UpdateWebhook(Webhook) -> WebhookResponse` - Update webhook config
- `DeleteWebhook(WebhookID) -> DeleteResponse` - Remove a webhook
- `ListWebhooks(AccountID) -> WebhookList` - List registered webhooks
- `GetDeliveryHistory(WebhookID) -> DeliveryList` - Delivery attempts with success/failure

**ConfigService (Gateway - manages live config)**
- `GetConfig(ServiceFilter) -> ConfigList` - List config keys, optionally filtered by service
- `UpdateConfig(ConfigUpdate) -> ConfigResponse` - Update a config value (writes to Postgres + publishes invalidation)
- `ResetConfig(Key) -> ConfigResponse` - Reset a key to its default value
- `GetConfigHistory(Key) -> ConfigHistoryList` - Change history for a key (links to audit log)

**SSAIService (SSAI Stitcher)**
- `CreateSession(SessionRequest) -> SessionResponse` - create viewer session for manifest stitching
- `GetSession(SessionID) -> Session` - session state (ads seen, breaks, frequency caps)
- `ListActiveSessions(Filter) -> SessionList` - active sessions (monitoring)
- `EndSession(SessionID) -> EndResponse` - explicitly end a session

**TranscoderService (Transcoder)**
- `GetTranscodeStatus(CreativeID) -> TranscodeStatus` - transcoding progress and variants produced
- `RetriggerTranscode(CreativeID) -> TranscodeResponse` - re-transcode (after spec change)
- `ListPendingJobs() -> JobList` - pending/active transcoding jobs (monitoring)

**CleanRoomService (Gateway or dedicated)**
- `CreateCleanRoom(CleanRoomRequest) -> CleanRoomResponse`
- `ApproveCleanRoom(ApprovalRequest) -> ApprovalResponse`
- `GetResults(CleanRoomID) -> CleanRoomResults`
- `RunComputation(ComputationType, CleanRoomID) -> ComputationResult`

**MarketplaceService (Gateway or dedicated)**
- `ListListings(MarketplaceFilter) -> ListingList`
- `CreateListing(Listing) -> ListingResponse`
- `EstimateExpansion(ListingID, BuyerAudienceID) -> ExpansionEstimate`
- `PurchaseAccess(ListingID, BillingModel) -> PurchaseResponse`
- `ProposeBarter(BarterProposal) -> BarterResponse`
- `GetFairnessScore(BarterID) -> FairnessScore`

**DemandSourceService (SSP)**
- `RegisterDemandSource(DemandSource) -> DemandSourceResponse` - add external DSP
- `UpdateDemandSource(DemandSourceID, Update) -> DemandSourceResponse`
- `DeleteDemandSource(DemandSourceID) -> DeleteResponse`
- `ListDemandSources(PublisherID) -> DemandSourceList`
- `GetDemandSourcePerformance(DemandSourceID) -> PerformanceReport`

### OpenRTB Endpoints (Bidding - JSON/HTTP)

These follow the OpenRTB 2.6 spec. Our exchange acts as both a receiver (from SSPs) and a sender (to DSPs).

**Exchange receives from SSP:**
```
POST /v1/openrtb/auction
Body: OpenRTB BidRequest (JSON)
Response: OpenRTB BidResponse (JSON)
```
- Fields supported: imp (id, banner, floor), site (domain, page), device (ua, ip, geo), user (id, segments)

**Exchange sends to DSP:**
```
POST /v1/openrtb/bid
Body: OpenRTB BidRequest (JSON)
Response: OpenRTB BidResponse (JSON)
```
- DSP responds with seatbid array containing bids, or empty response for no-bid
- Exchange enforces a strict timeout (e.g. 100ms) - no response in time = no-bid

**Win notice:**
```
GET /v1/openrtb/win?price=${AUCTION_PRICE}&bid_id={bid_id}
```
- Exchange notifies winning DSP of the clearing price

**Loss notice:**
```
GET /v1/openrtb/loss?bid_id={bid_id}&reason={loss_reason_code}&clearing_price={price}
```
- Exchange notifies losing DSPs why they lost

Loss reason codes (OpenRTB standard):

| Code | Reason | DSP action |
|---|---|---|
| 100 | Bid below floor price | Increase bid or skip placement |
| 101 | Bid below deal floor | Increase bid for this deal |
| 102 | Lost to higher bid | `clearing_price` shows how much higher the winner bid |
| 103 | Blocked by publisher controls | Stop bidding on this placement |
| 104 | Creative not approved | Fix or replace creative |
| 2 | Impression opportunity expired (timeout) | Respond faster |

Loss data is published to NATS (`adtech.auction.complete` already includes all bids and winner) and consumed by the **bid optimisation pipeline** to adjust bid strategies automatically.

### Tracker Endpoints (HTTP - External Facing)

These URLs are embedded in ad creatives and hit by end-user browsers. They must be fast, lightweight, and return minimal data.

```
GET /v1/t/imp?tid={trace_id}&cid={campaign_id}&pid={placement_id}&sig={signature}
-> 1x1 transparent pixel (HTTP 200, image/gif)
```

```
GET /v1/t/click?tid={trace_id}&cid={campaign_id}&pid={placement_id}&sig={signature}&redir={encoded_landing_url}
-> HTTP 302 redirect to landing page
```

```
GET /v1/t/conv?tid={trace_id}&cid={campaign_id}&type={conversion_type}&sig={signature}
-> 1x1 transparent pixel (HTTP 200, image/gif)
```

```
GET /v1/t/view?tid={trace_id}&cid={campaign_id}&pid={placement_id}&sig={signature}&dur={duration_ms}&pct={percent_visible}
-> HTTP 204 No Content
```

- `sig` is a signed hash to prevent spoofed events
- `dur` is how long the ad was continuously visible (milliseconds)
- `pct` is the percentage of pixels that were in viewport
- All endpoints append the event to NATS before responding

### Viewability (adtech.js SDK)

The publisher ad tag (`web/static/adtech.js`) includes a viewability monitor that runs client-side after the ad renders.

**How it works:**

```
Ad rendered in browser
    |
    v
adtech.js starts IntersectionObserver on the ad element
    |
    v
Monitors continuously:
    - What % of ad pixels are in the viewport?
    - How long has it been continuously visible?
    |
    v
50%+ visible for 1+ second (IAB/MRC standard)?
    |
    +-- Yes -> Fire viewability beacon: GET /v1/t/view?dur=1200&pct=85&...
    |          Tracker records: viewable impression
    |
    +-- No  -> No beacon. Impression recorded but not viewable.
```

**Three event states per ad:**

| State | What it means | Fired by | Always fires? |
|---|---|---|---|
| Served | Ad HTML delivered to browser | Ad Server response | Yes |
| Impression | Ad rendered in DOM | Impression pixel (`/v1/t/imp`) | Yes (if rendered) |
| Viewed | Human actually saw it (IAB standard met) | Viewability beacon (`/v1/t/view`) | Only if viewable |

**Viewability metrics in reporting:**

| Metric | Calculation |
|---|---|
| Viewability rate | Viewed impressions / total impressions |
| Average view duration | Mean `dur` across viewed impressions |
| Average visible area | Mean `pct` across viewed impressions |

**Billing impact:**

| Model | How viewability affects billing |
|---|---|
| CPM | All impressions billed (standard) |
| vCPM (viewable CPM) | Only viewed impressions billed - premium pricing |
| CPC/CPA | Viewability is a quality signal, not a billing trigger |

vCPM is a campaign-level setting. If enabled, only impressions with a matching viewability beacon are billable.

**Publisher quality signal:**

Placements with high viewability rates get higher bids from the smart router (Phase 3). Low viewability placements may indicate:
- Bad placement position (below the fold)
- Lazy loading issues
- Suspicious traffic (fraud)

Viewability rate per placement is surfaced in the publisher dashboard and feeds into the placement scoring optimisation pipeline.

### Gateway HTTP Endpoints (Consolidated)

The Gateway is the single HTTP entry point for all REST/dashboard traffic. Every endpoint below proxies to an internal gRPC service. The routing table shows which service owns each group.

**Insertion Orders -> DSP InsertionOrderService:**
- `GET    /v1/api/insertion-orders` - list IOs
- `POST   /v1/api/insertion-orders` - create IO
- `GET    /v1/api/insertion-orders/{id}` - get IO
- `PUT    /v1/api/insertion-orders/{id}` - update IO
- `DELETE /v1/api/insertion-orders/{id}` - delete IO
- `GET    /v1/api/insertion-orders/{id}/line-items` - list line items under this IO
- `GET    /v1/api/insertion-orders/{id}/budget-status` - budget spent/remaining/pacing

**Campaigns (Line Items) -> DSP CampaignService:**
- `GET    /v1/api/campaigns` - list campaigns
- `POST   /v1/api/campaigns` - create campaign
- `POST   /v1/api/campaigns/bulk` - bulk create campaigns
- `GET    /v1/api/campaigns/{id}` - get campaign
- `PUT    /v1/api/campaigns/{id}` - update campaign
- `PUT    /v1/api/campaigns/bulk` - bulk update campaigns
- `DELETE /v1/api/campaigns/{id}` - delete campaign
- `POST   /v1/api/campaigns/{id}/submit` - submit for review
- `POST   /v1/api/campaigns/{id}/approve` - approve (platform)
- `POST   /v1/api/campaigns/{id}/reject` - reject with reason (platform)
- `POST   /v1/api/campaigns/{id}/pause` - pause campaign
- `POST   /v1/api/campaigns/{id}/resume` - resume campaign
- `POST   /v1/api/campaigns/{id}/end` - end campaign
- `POST   /v1/api/campaigns/{id}/archive` - archive campaign
- `GET    /v1/api/campaigns/{id}/targeting` - get targeting rules
- `PUT    /v1/api/campaigns/{id}/targeting` - update targeting rules
- `GET    /v1/api/campaigns/{id}/schedule` - get scheduling (dayparting, flight dates)
- `PUT    /v1/api/campaigns/{id}/schedule` - update scheduling
- `GET    /v1/api/campaigns/{id}/creatives` - list attached creatives
- `POST   /v1/api/campaigns/{id}/creatives/{cid}/attach` - attach creative
- `POST   /v1/api/campaigns/{id}/creatives/{cid}/detach` - detach creative
- `PUT    /v1/api/campaigns/{id}/creatives/{cid}/weight` - set rotation weight
- `POST   /v1/api/campaigns/{id}/creatives/{cid}/promote` - promote A/B winner
- `GET    /v1/api/campaigns/{id}/creatives/performance` - creative A/B results
- `GET    /v1/api/targeting/options` - available targeting options (geos, devices, segments)
- `GET    /v1/api/campaigns/{id}/recommendations` - recommendations for a line item
- `GET    /v1/api/insertion-orders/{id}/recommendations` - IO-level recommendations
- `POST   /v1/api/campaigns/{id}/recommendations/{rid}/apply` - one-click apply
- `POST   /v1/api/campaigns/{id}/recommendations/{rid}/dismiss` - dismiss

**Creatives -> Ad Server AdService:**
- `GET    /v1/api/creatives` - list creatives
- `POST   /v1/api/creatives` - upload creative
- `POST   /v1/api/creatives/bulk` - bulk upload
- `GET    /v1/api/creatives/{id}` - get creative
- `PUT    /v1/api/creatives/{id}` - update creative metadata
- `DELETE /v1/api/creatives/{id}` - delete creative
- `GET    /v1/api/creatives/{id}/status` - review status
- `POST   /v1/api/creatives/{id}/approve` - approve (platform)
- `POST   /v1/api/creatives/{id}/reject` - reject with reason (platform)

**Publishers -> SSP InventoryService:**
- `GET    /v1/api/publishers` - list publishers
- `POST   /v1/api/publishers` - register publisher
- `GET    /v1/api/publishers/{id}` - get publisher
- `PUT    /v1/api/publishers/{id}` - update publisher
- `DELETE /v1/api/publishers/{id}` - deactivate publisher
- `GET    /v1/api/publishers/{id}/placements` - list placements
- `POST   /v1/api/publishers/{id}/placements` - add placement
- `POST   /v1/api/publishers/{id}/placements/bulk` - bulk add/update placements
- `GET    /v1/api/publishers/{id}/placements/{pid}` - get placement
- `PUT    /v1/api/publishers/{id}/placements/{pid}` - update placement
- `DELETE /v1/api/publishers/{id}/placements/{pid}` - deactivate placement
- `GET    /v1/api/publishers/{id}/placements/{pid}/tag` - get ad tag / JS snippet
- `POST   /v1/api/publishers/{id}/placements/{pid}/verify` - test integration
- `GET    /v1/api/publishers/{id}/placements/{pid}/health` - integration health (last request, fill rate, errors)
- `GET    /v1/api/publishers/{id}/placements/{pid}/floors` - get floor price config
- `PUT    /v1/api/publishers/{id}/placements/{pid}/floors` - set/update floor prices
- `PUT    /v1/api/publishers/{id}/placements/bulk-floors` - bulk update floor prices
- `GET    /v1/api/publishers/{id}/placements/{pid}/floor-recommendations` - dynamic floor recommendations
- `GET    /v1/api/publishers/{id}/placements/{pid}/forecast` - inventory forecast
- `GET    /v1/api/publishers/{id}/fill-rate` - fill rate per placement
- `GET    /v1/api/publishers/{id}/revenue` - revenue data
- `GET    /v1/api/publishers/{id}/health` - all placements integration health
- `GET    /v1/api/publishers/{id}/yield` - yield analytics (fill rate, eCPM, bid density, floor analysis)
- `GET    /v1/api/publishers/{id}/yield/recommendations` - yield optimisation suggestions
- `GET    /v1/api/publishers/{id}/demand-sources` - list connected demand sources
- `POST   /v1/api/publishers/{id}/demand-sources` - register external demand source (DSP endpoint, API key)
- `PUT    /v1/api/publishers/{id}/demand-sources/{did}` - update demand source config
- `DELETE /v1/api/publishers/{id}/demand-sources/{did}` - remove demand source

**Quality Controls -> SSP QualityControlService:**
- `GET    /v1/api/publishers/{id}/quality-controls` - list all quality controls
- `POST   /v1/api/publishers/{id}/quality-controls` - add a quality control rule
- `PUT    /v1/api/publishers/{id}/quality-controls/{qid}` - update a rule
- `DELETE /v1/api/publishers/{id}/quality-controls/{qid}` - remove a rule
- `POST   /v1/api/publishers/{id}/quality-controls/sync` - bulk sync from external system

**Deals -> SSP DealService:**
- `GET    /v1/api/deals` - list deals
- `POST   /v1/api/deals` - create deal (type, advertisers, price, volume, dates)
- `GET    /v1/api/deals/{id}` - get deal
- `PUT    /v1/api/deals/{id}` - update deal terms
- `DELETE /v1/api/deals/{id}` - cancel deal
- `GET    /v1/api/deals/{id}/performance` - deal performance (fill, revenue, volume)

**Audiences -> DSP AudienceService:**
- `GET    /v1/api/audiences` - list audience segments
- `POST   /v1/api/audiences/upload` - upload hashed customer list (CRM)
- `POST   /v1/api/audiences/segment` - create custom segment from rules
- `POST   /v1/api/audiences/lookalike` - create lookalike from seed audience
- `GET    /v1/api/audiences/{id}` - get audience segment
- `PUT    /v1/api/audiences/{id}` - update audience segment
- `DELETE /v1/api/audiences/{id}` - delete audience segment
- `GET    /v1/api/audiences/{id}/size` - audience match size
- `GET    /v1/api/audiences/{id}/analytics` - composition, growth, demographics
- `GET    /v1/api/audiences/overlap?ids=` - overlap analysis between segments
- `GET    /v1/api/audiences/{id}/reachable?days=` - active reachable users

**Pixels -> Tracker / Ad Server:**
- `GET    /v1/api/pixels` - list data collection pixels
- `POST   /v1/api/pixels` - create new pixel
- `GET    /v1/api/pixels/{id}/code` - get HTML/JS snippet
- `GET    /v1/api/pixels/{id}/status` - is pixel firing? Last seen, events/day

**Reporting -> ReportingService:**
- `GET    /v1/api/reports/campaigns?from=&to=&group_by=` - campaign performance
- `GET    /v1/api/reports/publishers?from=&to=&group_by=` - publisher revenue
- `GET    /v1/api/reports/trace/{trace_id}` - full trace of a single ad request
- `GET    /v1/api/reports/builder` - available metrics, dimensions, filters
- `POST   /v1/api/reports/query` - run a custom report
- `POST   /v1/api/reports/saved` - save a report template
- `GET    /v1/api/reports/saved` - list saved reports
- `PUT    /v1/api/reports/saved/{id}` - update saved report (including schedule)
- `DELETE /v1/api/reports/saved/{id}` - delete saved report
- `POST   /v1/api/reports/jobs` - submit an async report job (template or ad-hoc; reports:export)
- `GET    /v1/api/reports/jobs` - list the account's report jobs
- `GET    /v1/api/reports/jobs/{id}` - job status + artifact metadata
- `GET    /v1/api/reports/jobs/{id}/download` - stream the CSV/JSON/Parquet artifact (reports:export)

**Billing -> BillingService:**
- `GET    /v1/api/billing/balance` - current advertiser balance
- `POST   /v1/api/billing/topup` - add funds to account
- `POST   /v1/api/billing/credit-request` - request credit terms
- `PUT    /v1/api/billing/alerts` - configure low balance alert threshold
- `GET    /v1/api/billing/invoices` - invoice list
- `GET    /v1/api/billing/invoices/{id}` - invoice detail with line-item breakdown
- `GET    /v1/api/billing/invoices/{id}/report` - downloadable invoice report (CSV/PDF)
- `GET    /v1/api/earnings/summary` - publisher earnings overview
- `GET    /v1/api/earnings/payouts` - payout list
- `GET    /v1/api/earnings/payouts/{id}/report` - downloadable payout report
- `GET    /v1/api/reconciliation/daily?date=` - daily reconciliation results (platform)
- `GET    /v1/api/reconciliation/flagged` - campaigns with discrepancies (platform)

**Fraud -> FraudService:**
- `GET    /v1/api/fraud/overview` - traffic quality overview
- `GET    /v1/api/fraud/trends?from=&to=` - fraud rate trends over time
- `GET    /v1/api/fraud/flagged?from=&to=` - flagged events with drill-down
- `GET    /v1/api/fraud/publishers/{id}/quality` - publisher quality score
- `GET    /v1/api/fraud/ivt-report?from=&to=` - invalid traffic report (advertiser-facing)
- `GET    /v1/api/fraud/blocklists` - view current blocklists
- `POST   /v1/api/fraud/blocklists` - add to blocklists (IPs, UAs, domains)
- `DELETE /v1/api/fraud/blocklists/{id}` - remove from blocklists

**Pipeline -> PipelineService:**
- `GET    /v1/api/pipeline/files` - list ingested files with status
- `GET    /v1/api/pipeline/files/{id}` - file processing status
- `POST   /v1/api/pipeline/files` - upload publisher data file
- `GET    /v1/api/pipeline/quarantine` - quarantined files/rows
- `POST   /v1/api/pipeline/quarantine/{id}/reprocess` - reprocess quarantined file
- `GET    /v1/api/pipeline/drift/{publisher_id}` - schema drift history
- `POST   /v1/api/pipeline/drift/{id}/approve` - approve a drift mapping
- `GET    /v1/api/pipeline/queue` - queue depth / processing status

**Audit -> AuditService:**
- `GET    /v1/api/audit?resource_type=&resource_id=` - per-resource audit trail
- `GET    /v1/api/audit?actor_id=` - per-user activity log
- `GET    /v1/api/audit/search?q=` - search audit events
- `GET    /v1/api/audit/export?from=&to=&format=csv` - export audit trail

**Config -> ConfigService (Gateway-local):**
- `GET    /v1/api/config` - list config keys (optionally filter by service)
- `PUT    /v1/api/config/{key}` - update a config value
- `POST   /v1/api/config/{key}/reset` - reset to default
- `GET    /v1/api/config/{key}/history` - change history

**Operations -> Gateway-local (K8s API + live config):**
- `GET    /v1/api/ops/deployments` - current version per service
- `GET    /v1/api/ops/ab-tests` - list active and past A/B tests
- `POST   /v1/api/ops/ab-tests` - create A/B test
- `PUT    /v1/api/ops/ab-tests/{id}` - update split %, end test
- `GET    /v1/api/ops/ab-tests/{id}/metrics` - live comparison metrics
- `POST   /v1/api/ops/canary/{service}/promote` - promote canary
- `POST   /v1/api/ops/canary/{service}/rollback` - rollback canary

**Moderation -> Various services (platform admin only):**
- `GET    /v1/api/moderation/queue` - pending items by type
- `POST   /v1/api/moderation/{type}/{id}/approve` - approve item
- `POST   /v1/api/moderation/{type}/{id}/reject` - reject with reason

**Identity & Privacy -> IdentityService:**
- `POST   /v1/api/privacy/opt-out` - user opt-out
- `POST   /v1/api/privacy/delete` - right-to-deletion request
- `GET    /v1/api/identity/{user_id}/graph` - identity graph (platform admin)

**Onboarding -> Gateway-local:**
- `GET    /v1/api/onboarding/status` - current onboarding step
- `PUT    /v1/api/onboarding/step/{step}` - complete an onboarding step

**Auth & Account -> AccountService (Gateway-local):**
- `POST   /v1/api/auth/register` - register new account
- `POST   /v1/api/auth/verify-email` - verify email address
- `POST   /v1/api/auth/login` - login, returns JWT
- `POST   /v1/api/auth/refresh` - refresh JWT token
- `POST   /v1/api/auth/forgot-password` - initiate password reset
- `POST   /v1/api/auth/reset-password` - complete password reset
- `GET    /v1/api/account` - get current account details
- `PUT    /v1/api/account` - update account
- `GET    /v1/api/account/team` - list team members
- `POST   /v1/api/account/team` - invite team member
- `PUT    /v1/api/account/team/{id}` - update team member role
- `DELETE /v1/api/account/team/{id}` - remove team member

**API Keys -> AccountService:**
- `GET    /v1/api/keys` - list API keys
- `POST   /v1/api/keys` - generate new API key
- `DELETE /v1/api/keys/{id}` - revoke API key

**Webhooks -> WebhookService:**
- `GET    /v1/api/webhooks` - list registered webhooks
- `POST   /v1/api/webhooks` - register webhook
- `PUT    /v1/api/webhooks/{id}` - update webhook
- `DELETE /v1/api/webhooks/{id}` - remove webhook
- `GET    /v1/api/webhooks/{id}/deliveries` - delivery history

**Notifications -> Gateway-local:**
- `GET    /v1/api/notifications` - list notifications
- `PUT    /v1/api/notifications/{id}/read` - mark as read
- `GET    /v1/api/notifications/unread-count` - unread count
- `GET    /v1/api/notifications/preferences` - get preferences
- `PUT    /v1/api/notifications/preferences` - update preferences

**File Upload / Export:**
- `POST   /v1/api/upload/creative` - upload creative asset -> proxies to AdService
- `POST   /v1/api/upload/publisher-data` - upload data file -> proxies to PipelineService
- `GET    /v1/api/export/{type}/{id}` - download report/invoice/payout as CSV/PDF

**Static:**
- `GET    /sellers.json` - auto-generated authorised sellers list
- `GET    /docs` - Swagger UI (OpenAPI spec)

### Route Testing

Three automated tests prevent dead routes and missing proxies:

```go
func TestAllRoutesHaveGRPCBackend(t *testing.T) {
    // Every HTTP route in the Gateway router must map to
    // an existing gRPC service and method in pkg/proto/
}

func TestAllGRPCMethodsAreRouted(t *testing.T) {
    // Every gRPC method in proto definitions (except allowlisted
    // internal-only methods) must have a corresponding HTTP route
}

func TestOpenAPISpecMatchesRoutes(t *testing.T) {
    // Every endpoint in docs/openapi.yaml must be registered
    // in the Gateway router
}
```

CI fails if routes and gRPC definitions are out of sync. No dead endpoints, no missing proxies.

### NATS Subjects (Async Events)

All messages are protobuf-encoded. Subjects follow the pattern `adtech.{domain}.{event_type}`.

#### Event Subjects (JetStream - persistent, ack-based)

| Subject | Publisher | Subscriber(s) | Payload |
|---|---|---|---|
| `adtech.events.impression` | Tracker | Reporting, DSP (budget) | ImpressionEvent |
| `adtech.events.click` | Tracker | Reporting (analytics + CPC settle) | ClickEvent |
| `adtech.events.view` | Tracker | Reporting (analytics + vCPM settle) | ViewabilityEvent |
| `adtech.events.conversion` | Tracker | Reporting (analytics + CPA settle) | ConversionEvent |
| `adtech.auction.win` | Exchange | DSP (budget reservation), Reporting (analytics + billing accrual in one consumer) | AuctionWinEvent (single source of truth for cost) |
| `adtech.auction.complete` | Exchange | Reporting | AuctionCompleteEvent (includes all bids, winner, timing) |
| `adtech.datafee.observed` | SSP | Reporting (data-monetization accrual) | DataFeeEvent (external win on a request carrying fee-bearing segments; parked in data_fee_pending, accrues at impression — owner credited net of reporting.data_fee_margin_pct, external seat accrues a receivable) |
| `adtech.budget.depleted` | DSP | Exchange (stop bidding for this campaign) | BudgetDepletedEvent |
| `adtech.balance.depleted` | DSP (bid gate), Reporting (billing sink) | Webhooks, Reporting | BalanceDepletedEvent (advertiser prepay balance hit zero — account-wide no-bid until topup) |
| `adtech.billing.campaign_spend_snapshot` | Reporting (billing engine, periodic) | DSP (all pods, fan-out) | CampaignSpendSnapshotEvent (per-campaign committed spend = settled + open reserves, in cents; DSPs reconcile pacing counters to it) |
| `adtech.cache.invalidate.advertiser-balances` | Gateway (topup credit), Reporting (spend drawdown, throttled per account) | DSP balance warm cache | invalidate ping (cache reloads wholesale) |
| `adtech.campaign.state_changed` | DSP | Reporting, Webhooks | CampaignStateEvent (lifecycle transitions) |
| `adtech.creative.review_completed` | Ad Server | Gateway (notifications), Webhooks | CreativeReviewEvent |
| `adtech.billing.reservation_created` | Billing | Reporting | ReservationEvent (CPC/CPA budget hold) |
| `adtech.billing.reservation_settled` | Billing | Reporting | ReservationEvent (click/conversion arrived) |
| `adtech.billing.reservation_released` | Billing | Reporting | ReservationEvent (no click/conversion, hold released) |
| `adtech.privacy.opt_out` | Gateway | DSP, Ad Server, Tracker, Reporting, Identity service | OptOutEvent (level 1 or 2, immediate) |
| `adtech.privacy.deletion_requested` | Gateway | Deletion job runner (`cmd/privacy-delete/`) | DeletionEvent (level 3, queues async deletion) |
| `adtech.privacy.deletion_completed` | Deletion job | Gateway (dashboard), Audit | DeletionCompletedEvent (systems purged, verification status) |
| `adtech.identity.observed` | SSP (ad-tag requests) + Exchange (inbound Prebid), opt-in | Identity-consumer (`cmd/identity-consumer/`) | ObservedEvent{ids:[{value,source}], fingerprint} — the consumer builds identity_graph edges (deterministic co-occurrence + probabilistic IP+UA). Best-effort. Any service that sees identity signals can publish via `pkg/identityobserve.Publisher`. |
| `adtech.profile.signal` | Gateway (audience uploads: portal CSV + API JSON) | Pipeline (datalake sink → `profile_signals` Delta table) | ProfileSignalEvent{account_id, source, access, segment_id/name, visibility, consent, ids:[{id_type,id_value}]} — BATCHED (~1000 ids/message). The append-only lake record that makes segment memberships recomputable. Drop-zone files skip NATS: the pipeline writes the same rows directly. |
| `adtech.behaviour.observed` | SSP (request rows, consent-gated at capture via privacy.Evaluate) + Tracker (impression/click/conversion/view rows, gated by the consented uid= the ad server bakes into beacons) | Pipeline (datalake sink → `behaviour_signals` Delta table) | BehaviourSignalEvent{kind, user_id/household_id, placement, publisher, campaign, creative, channel, categories (stamped at event time), geo, device} — input to behavioural segmentation (cmd/profile-builder). Never carries non-consented users. |
| `adtech.batch.run_completed` | batch-conductor (hourly CronJob) | Webhooks / ops dashboards | One event per data-chain run: {run_id, aborted, failed_steps, duration_ms}. The chain (checkpoint → compact → rollups → profile-builder → privacy delete → verify) is explicit completion-ordered code in `pkg/batch` — it replaced the time-staggered CronJob lattice; per-step outcomes live in the `batch_runs` table (staff → Batch runs). |
| `adtech.events.video` | Tracker / SSAI beacon server | Reporting (analytics + billing) | VideoEvent (start, quartiles, complete, skip) |
| `adtech.events.audio` | Tracker / SSAI beacon server | Reporting (analytics + billing) | AudioEvent (start, quartiles, complete) |
| `adtech.events.dooh` | Screen proof-of-play | Reporting (analytics + billing) | DOOHEvent{screen_id, plays, estimated_audience} |
| `adtech.events.retail` | Retail site tracker | Reporting (analytics + billing) | RetailEvent{product_id, query, position, conversion} |
| `adtech.events.reward` | Game SDK via Tracker | Reporting (analytics + billing) | RewardEvent{trace_id, reward_type, verified} |
| `adtech.events.ingame` | Game SDK via Tracker | Reporting, Billing | InGameEvent{trace_id, event_type: viewable, billboard_id} |
| `adtech.events.install` | Game/app SDK postback | Reporting (analytics + CPI settle) | InstallEvent{trace_id, app_bundle, verified} |
| `adtech.video.transcode_requested` | Ad Server | Transcoder | TranscodeRequest{creative_id, variants_needed} |
| `adtech.video.transcode_completed` | Transcoder | Ad Server | TranscodeComplete{creative_id, variants_produced} |
| `adtech.audio.transcode_requested` | Ad Server | Transcoder | AudioTranscodeRequest{creative_id} |
| `adtech.audio.transcode_completed` | Transcoder | Ad Server | AudioTranscodeComplete{creative_id, variants} |
| `adtech.video.session_started` | SSAI | Reporting | SessionStartEvent{session_id, viewer, content} |
| `adtech.video.session_ended` | SSAI | Reporting | SessionEndEvent{session_id, duration, breaks_seen} |
| `adtech.billing.tier_changed` | Billing | Reporting, Gateway (dashboard notification) | TierChangedEvent{publisher_id, old_tier, new_tier} |
| `adtech.audience.membership_updated` | Audience store ingest | Postgres writer (durable backup) | MembershipEvent{user_id, segments_added, segments_removed} |
| `adtech.cleanroom.requested` | Gateway | Clean room job runner | CleanRoomRequest |
| `adtech.cleanroom.completed` | Job runner | Gateway (notifications), Audit | CleanRoomResults |
| `adtech.marketplace.purchased` | Gateway | Billing, Audience activation | PurchaseEvent |
| `adtech.marketplace.barter.accepted` | Gateway | Audience activation (both parties) | BarterAcceptedEvent |
| `adtech.marketplace.revoked` | Data provider (Gateway) | Audience store (deactivate segment for buyer) | RevocationEvent |
| `adtech.pipeline.file.ingested` | Pipeline | Reporting | FileIngestedEvent (new publisher file processed) |
| `adtech.pipeline.file.quarantined` | Pipeline | Gateway (dashboard alerts) | FileQuarantinedEvent (validation failed) |
| `adtech.pipeline.drift.detected` | Pipeline | Gateway (dashboard alerts) | SchemaDriftEvent |

#### Consumer Naming Convention (load-bearing)

JetStream consumers are identified by name. Two `CreateOrUpdateConsumer` calls with the same name **overwrite** each other's `FilterSubject` rather than coexisting. A service that subscribes to multiple subjects with a single "group" identifier silently collapses into one consumer whose filter is whichever subject won the race.

The natsbus wrapper (`pkg/events/natsbus.Subscribe`) therefore derives the consumer name from `service + group + subject-leaf`:

```
service="reporting", group="reporting", subject="adtech.events.impression"
  → consumer name "reporting-reporting-impression"

service="reporting", group="reporting", subject="adtech.events.click"
  → consumer name "reporting-reporting-click"
```

Each subject gets its own consumer, its own `FilterSubject`, and its own fetch goroutine. Callers cannot collide regardless of how they pick group names. **Do not bypass this** — past incident: an early `cmd/reporting` version subscribed all four event subjects under group="reporting" without subject in the name. Only the last subject won; the other three were silently dropped by JetStream's `InterestPolicy` retention (no interested consumer = no retention), and the dispatching goroutines mis-decoded the surviving subject's payloads as the wrong event types. Symptom was ledger entries with valid `TraceID` but empty `CampaignID` and zero `Amount`. Hard to spot because Go map iteration order randomised which subject "won" each boot.

#### Webhook Subjects (JetStream - reliable delivery)

| Subject | Publisher | Subscriber(s) | Payload |
|---|---|---|---|
| `adtech.webhooks.budget.depleted` | DSP | Webhook dispatcher | WebhookEvent |
| `adtech.webhooks.campaign.ended` | DSP | Webhook dispatcher | WebhookEvent |
| `adtech.webhooks.campaign.approved` | Gateway | Webhook dispatcher | WebhookEvent |
| `adtech.webhooks.invoice.generated` | Billing | Webhook dispatcher | WebhookEvent |
| `adtech.webhooks.payout.processed` | Billing | Webhook dispatcher | WebhookEvent |
| `adtech.webhooks.fraud.alert` | Fraud batch job | Webhook dispatcher | WebhookEvent |
| `adtech.webhooks.schema.drift` | Pipeline | Webhook dispatcher | WebhookEvent |
| `adtech.webhooks.report.ready` | Report scheduler | Webhook dispatcher | WebhookEvent |

#### Cache Invalidation Subjects (Core NATS - fire-and-forget, not JetStream)

| Subject | Publisher | Subscriber(s) | Payload |
|---|---|---|---|
| `adtech.cache.invalidate.campaigns` | DSP | All DSP instances | CacheInvalidation{campaign_id} |
| `adtech.cache.invalidate.creatives` | Ad Server | All Ad Server instances | CacheInvalidation{creative_id} |
| `adtech.cache.invalidate.placements` | SSP | All Exchange, SSP instances | CacheInvalidation{placement_id} |
| `adtech.cache.invalidate.dsp-endpoints` | Gateway | All Exchange instances | CacheInvalidation{} |
| `adtech.cache.invalidate.keys` | Gateway | All Tracker instances | CacheInvalidation{} |
| `adtech.cache.invalidate.config` | Gateway | All service instances | CacheInvalidation{key} |
| `adtech.cache.invalidate.house-ads` | Gateway (staff house-ad CRUD) | Publisher Ad Server instances | CacheInvalidation{id} |
| `adtech.cache.invalidate.router-stats` | Exchange (debug routing reset) | All Exchange instances | {} — each pod wipes its SmartRouter and rebases its reseed watermark |

Trace IDs are included in every JetStream message so events can be correlated across subjects. Cache invalidation messages are fire-and-forget (worst case: one stale cache cycle).

---

## Data Storage

### Storage Layers

| Layer | Technology | What lives here |
|---|---|---|
| Transactional | PostgreSQL | Accounts, campaigns, creatives, publishers, placements, budgets, targeting rules. ACID transactions for things like budget deduction. |
| Analytics | DuckDB or ClickHouse (pluggable) | Impressions, clicks, conversions, auction logs. High-volume append-only event data queried by aggregation (sum, count, group by). |
| Object storage | S3 everywhere - Minio locally, real S3 in staging/prod. One code path, just a different endpoint URL. | Creative assets (images, HTML bundles), exported reports (CSV), pipeline data files. |

### Analytics Store: DuckDB vs ClickHouse

Both are supported behind a shared interface in `pkg/store/analytics/`. The reporting service doesn't know which backend it's talking to.

**DuckDB (default for local and staging):**
- Embedded - runs inside the reporting service process, no extra infrastructure
- Zero setup - just a file on disk, backed by a K8s PersistentVolumeClaim so data survives pod restarts
- Fast analytical queries over millions of rows
- Perfect for local dev where you don't want another container running
- Single-writer only - reporting service must run as single replica when using DuckDB

**ClickHouse (option for prod or heavy workloads):**
- Separate server - runs as its own K8s deployment
- Scales to billions of rows, distributed
- Production-proven at ad tech scale
- Worth the extra infra when data volume justifies it

### Environment Defaults

| Environment | Transactional | Analytics | Object storage |
|---|---|---|---|
| Local | PostgreSQL | DuckDB (embedded) | Minio (S3-compatible, runs in K8s) |
| Staging | PostgreSQL | DuckDB or ClickHouse | S3 (dev bucket) |
| Prod | PostgreSQL | ClickHouse | S3 (prod bucket) |

Configured via Kustomize overlays - same code, different config per environment. Minio locally means the **same S3 code path runs everywhere** - no separate filesystem implementation to maintain. Just a different endpoint URL.

### Data Flow

```
Services (DSP, Exchange, Ad Server)
    |
    v
  Tracker ---> NATS (protobuf events)
                 |
                 v
             Reporting Service
                 |
          +------+------+
          |             |
       DuckDB      ClickHouse
     (embedded)     (server)
```

Events always flow through NATS. The reporting service consumes from NATS and writes to whichever analytics store is configured. This means adding a new sink later (Snowflake, BigQuery, a data lake) is just another NATS subscriber - nothing upstream changes.

### Persistence Strategy

This section is the build guide for closing the gap between "in-memory today" and "durable in prod." Every service holds state that survives across requests; this catalogues what each piece is, where it lives now, where it needs to live, and how it gets there safely.

#### State inventory (what services hold)

| State | Holder | Today | Target | Loss tolerance |
|---|---|---|---|---|
| **Ledger entries** (every spend, reservation, settlement) | reporting | `[]LedgerEntry` slice (volatile) | Postgres `ledger_entries` table (already in migrations) | **Zero.** Loss = unbilled spend, fails financial reconciliation. |
| **Analytics events** (impression, click, conversion, auction) | reporting | `analytics.NewMemory` slices (volatile) | DuckDB file on PVC (dev/staging) or ClickHouse (prod) | Low. Loss = missing rows in reports for the window between last persist and crash. |
| **Aggregated rollups** (hourly/daily campaign+placement metrics) | rollup job (not built) | — | Postgres `rollups_*` tables + Parquet on S3 | Low. Can be regenerated from raw events. |
| **Budget counters** (campaign spend-to-date) | DSP | Redis (`dsp:budget:{id}:spent`) | Same, with periodic Postgres snapshot for daily-close reconciliation | Low. Counter resets daily; worst case = one day of over-spend if Redis dies mid-window. |
| **Frequency caps** (user × campaign serve counts) | adserver | Redis (`adserver:freqcap:{user}:{campaign}`) | Same | Acceptable. Loss = users see ads 1-2x more than cap until reload. |
| **Tracker dedup** (seen trace IDs) | tracker | Redis (`tracker:seen:{type}:{trace}` SetNX) | Same | Acceptable. Loss = a small window where repeat impressions could double-count. |
| **Warm caches** (campaigns, placements, deals, creatives) | DSP/SSP/exchange/adserver | In-process `atomic.Pointer[snapshot]` (volatile) | Stays in-process — backed by Postgres reload + NATS invalidate. No persistence needed. | Built-in: every cache cold-starts from its Postgres loader. |
| **Audience segment memberships** | SSP/DSP | Postgres `audience_segment_members` (already persistent) | Add Redis hot-path cache in front | None — durable today, just slow. |
| **Bid shading stats** (per-placement clearing-price model) | DSP | `bidshading.Tracker` in-process map | Postgres `bid_shading_stats` (new table) + periodic snapshot | Medium. Loss = model resets to "no learning," need ~hours of bids to converge again. |
| **Smart router stats** (per-DSP bid rate, latency) | exchange | `optimise.SmartRouter` in-process map | Same as bid shading — new Postgres table, periodic snapshot | Medium. Same convergence concern. |
| **Audit log** | gateway/config-manager | Postgres `audit_log` (already persistent) | No change | None — durable today. |
| **Creative HTML + assets** | adserver | Postgres `creatives.html_content` for small + Minio/S3 for large | No change | None — durable today. |
| **Trace + log data** | every service | Loki (logs) + Jaeger (traces), PVC-backed | No change | Operational only; not financially or legally load-bearing. |

#### Write patterns by criticality

Three distinct patterns, picked by how badly a crash-between-write-and-flush hurts:

**Pattern A: synchronous-write-through (financial state).** Used for the ledger. The handler call returns only after Postgres has acked the INSERT. The in-memory slice stays as a read-through cache for `GET /v1/billing/summary` so the dashboard is fast, but the source of truth is the row in `ledger_entries`. If Postgres is unreachable, the handler **fails closed** — Nak the NATS message, let it redeliver. Better to slow down ingestion than to lose money silently.

```
handleImpression:
   1. ProcessEvent(...) → computes LedgerEntry
   2. INSERT INTO ledger_entries (...)        ← synchronous, blocks until acked
   3. ledger.entries = append(ledger.entries, entry)   ← cache only after DB ack
   4. msg.Ack()
```

**Pattern B: batched-async-write (high-volume event data).** Used for analytics events. Handler appends to an in-process buffer; a periodic flush worker writes batches to the analytics store (DuckDB or ClickHouse). NATS ack only happens once the batch is durable. Buffer flush is triggered by either size (N events) or time (T ms) — whichever first. Crash before flush = NATS redelivers from the last acked offset, so events are at-least-once delivered (idempotent via trace_id).

```
handleImpression:
   1. buffer.append(event)
   2. (no immediate ack — message stays unacked in JetStream)

flushWorker (every 200ms or 500 events):
   1. batch = buffer.drain()
   2. store.InsertBatch(batch)          ← single network roundtrip
   3. for each msg in batch: msg.Ack()  ← only ack after durable
```

**Pattern C: fire-and-forget (operational state).** Used for warm caches, dedup counters, shading/routing stats. State is rebuildable — if it's lost, the system recovers organically (caches reload from Postgres, stats relearn from incoming traffic, dedup just allows one duplicate). No write coordination needed; failures get a `log.Warn` and move on.

#### Recovery patterns

When a service starts (cold boot, restart, rolling deploy), it has to reconstruct its in-memory state. Three approaches in use:

| State | Recovery on startup |
|---|---|
| Warm caches | `Loader.LoadAll(ctx)` runs synchronously in `warm.Cache.Start` before the service accepts traffic. /readyz fails until the first load completes. |
| Ledger cache | `SELECT * FROM ledger_entries WHERE timestamp >= now() - interval '90 days' INTO MEMORY`. Bounded by retention window so memory doesn't grow unboundedly. Older rows accessed via direct SQL on demand. |
| Analytics events | Not loaded — the store IS the source of truth. Query API hits DuckDB/ClickHouse directly. |
| Shading + router stats | Load latest snapshot from Postgres on startup, continue learning forward. Snapshot every 5 minutes via a background goroutine. |
| Budget counters | Redis is the source of truth. If Redis is empty on cold start (cluster reboot), DSP reads "spent so far today" from `SELECT SUM(amount) FROM ledger_entries WHERE timestamp >= today` to rebuild. |
| Dedup keys | Acceptable to lose. Redis cold start = one window of possible duplicates. |

#### Failure modes — what happens when a backing store is unreachable

The whole system is built around "degrade, don't die." Each store has an explicit fallback policy. Encode this once in service boot, surface via /readyz so K8s can route around bad instances.

| Store | Service behavior when down | Why |
|---|---|---|
| Postgres (campaigns, deals, etc.) | Services keep serving from warm cache snapshot. /readyz still 200. Log WARN every retry. | Bid path must not stop because of DB blip. Stale data is better than no bids. |
| Postgres (ledger) | Reporting handler Nak's NATS message. /readyz reports degraded but not down. | Financial integrity > throughput. NATS holds messages for redelivery. |
| Redis (budget, freq cap, dedup) | Fail-open. Bid path continues, freq caps don't enforce, dedup allows duplicates. Log WARN. | Same logic as cached campaigns — better to serve than not. Reconciliation catches over-spend. |
| Analytics store (DuckDB/ClickHouse) | Buffer fills up to a bound (e.g. 10k events), then drops oldest with WARN. /readyz reports degraded. | Reports go stale but ad serving continues. Operational alert fires. |
| NATS | Tracker uses HTTP fallback (`/v1/reporting/events` direct POST). Cache invalidate is fire-and-forget so lost messages = one extra poll cycle of stale cache. | Tracker is the only thing on the critical path that MUST get its event somewhere. |
| Minio/S3 | Ad server falls back to `html_content` from Postgres for small banners. Large-asset creatives 503 cleanly. | Most creatives are inline today; large assets are rare. |

#### Implementation order (closing the gaps)

These map to the rows in the inventory table marked "today: volatile." Build order by financial / correctness impact:

1. **Billing ledger → Postgres (pattern A).** Schema (`ledger_entries`, `reservations`, `invoices`, `payouts`) is already in `migrations/`. Implement `pkg/billing.PostgresLedger` satisfying the same interface as `NewLedger`; wire `cmd/reporting/main.go` to use it. Load last 90 days on startup. Highest urgency — losing financial state on a pod restart is the worst class of bug we still have.
2. **CPC/CPA/vCPM dispatch from click/conversion/view handlers.** Reuses `billing.Engine.ProcessEvent` which already supports reserve/settle for non-CPM models. Unblocks 8 skipped `billing_models_test.go` cases.
3. **Analytics store → DuckDB by default in dev.** Add config key `analytics.backend = memory|duckdb|clickhouse`; existing `pkg/store/analytics/duckdb.go` is ready. Use a PVC so the file survives pod restarts. Switch `cmd/reporting/main.go:54` to read the config.
4. **Batched-async write worker for analytics (pattern B).** Today each event is one Postgres/DuckDB roundtrip; under load this dominates latency. Drain buffer into `InsertBatch` (already implemented on DuckDB).
5. **ClickHouse implementation** for multi-replica prod reporting (currently DuckDB blocks scaling reporting past 1 replica).
6. **Bid shading + smart router stat snapshots.** New tables (`bid_shading_stats`, `smart_router_stats`), background snapshot goroutine in each holder service, load-on-startup. Lower priority — current behavior is "warm up on every restart," which is fine while traffic volumes are low.
7. **Rollup job** reading from analytics store, writing aggregates back into Postgres `rollups_*` + Parquet onto S3 via Delta Log (see "Data Rollups" section).
8. **Win-notify reconciliation job** (new `cmd/reconcile` or as a sub-loop in `cmd/billing` once it exists). Periodically scans:
   - reporting analytics: all `auction_wins` in the last N minutes
   - DSP budget counters: cumulative spend per campaign
   Surfaces any (campaign, win) where the auction recorded a clearing price but the DSP budget didn't tick up by the matching amount. Catches transient HTTP nurl failures uniformly for all DSP classes (internal *and* external). This is the right replacement for the dual-transport DSP redundancy we deliberately did not build — symmetric, observable, doesn't fork the wire protocol.

Steps 1-2 are high urgency (financial correctness). 3-4 are needed before any meaningful load test (memory growth). 5-8 are scaling/optimisation work.

#### Published events with no consumer (events going to /dev/null)

A wider audit of NATS subjects on 2026-05-31 found one published-but-unconsumed subject and a long list of subjects spec'd in PLAN.md but never published yet. Listing the actively-broken cases:

| Subject | Published by | Spec'd consumer | Reality |
|---|---|---|---|
| `adtech.auction.win` | exchange (`cmd/exchange/main.go`) | Reporting (analytics today, billing ledger when migration lands) | **Reporting subscriber wired (2026-05-31).** DSPs do **not** subscribe — budget is updated via the OpenRTB HTTP nurl path. Billing flow still on impression-pixel path; flip is deferred (see migration plan below). |

#### Three transports, three roles (current state after 2026-05-31)

The exchange–DSP and exchange–reporting interactions are deliberately split so each transport carries a single responsibility. No redundant paths, no dedup machinery, identical behavior for internal and external DSPs.

| Transport | Direction | Role | Consumer |
|---|---|---|---|
| HTTP OpenRTB bid request/response | exchange ↔ DSP | Auction primitive: ask for a bid, get a bid back inside the timeout window | Every DSP (internal + external) |
| HTTP OpenRTB nurl (`/v1/openrtb/win`) | exchange → DSP | Tell the winning DSP "you won at price X" so it credits its budget counter | Winning DSP only (point-to-point) |
| NATS `adtech.auction.win` | exchange → internal bus | Internal event stream of auction outcomes for analytics, future billing ledger, future learning models | Reporting (and future webhooks, fraud scoring, etc.). **NOT DSPs.** |

Why DSPs don't subscribe to the NATS path:

- **Uniformity.** External DSPs (Xandr, DV360, TTD) can't reach our internal NATS bus — there's a security boundary. If internal DSPs subscribed to NATS for budget while external relied on HTTP, the two DSP classes would have different reliability profiles for the same financial signal. Operational confusion + asymmetric over-spend risk.
- **OpenRTB compliance.** The HTTP nurl is the IAB standard. As long as some DSPs must use it, all DSPs should use it.
- **No protocol fork.** Keeping all DSP-facing wire protocols on HTTP means the exchange has one bid-fan-out implementation, one win-notify implementation, one timeout strategy, one set of tests. Adding NATS as an alternative DSP transport would double that surface for marginal latency benefit at our scale.

What we lose by not having NATS as a DSP-side redundancy: a small reliability tail (HTTP win-notify can fail on transient errors / pod restarts, leaving a single win uncounted in the DSP's budget). The right fix is a **reconciliation job** (see "Persistence Strategy" → Implementation order), not parallel transport machinery. The job periodically diffs reporting's NATS-recorded auction-wins against DSP budget counters and surfaces discrepancies — that catches under-counts uniformly for every DSP class.

What `adtech.auction.win` does today:

- Reporting subscriber writes to `analytics.Store.InsertAuctionWin`. Every auction outcome (winner DSP, advertiser, campaign, clearing price, deal_id) lives in the analytics store with full lineage. Queryable via the standard analytics path and the `/debug/auction_wins?trace_id=` debug endpoint.

What it does **not** do (deferred — see migration plan below):

- Billing ledger entry. The impression-pixel handler still writes the spend entry for CPM. Flipping the billing source from impression to auction.win is a breaking change: it changes the semantic of "spend" from "delivered impression" to "won auction (regardless of delivery)" and requires the click/conversion/view handlers to switch to a settlement model. Migrating cleanly is the right move; doing it as a side effect of fixing the /dev/null issue would have been not.

#### Billing source-of-truth migration (deferred — recommended next)

The right end state per the architecture spec is:

```
auction.win → reservation entry in ledger (immediately, regardless of delivery)
    ├─ CPM model     → settled to "spend" by the impression event (impression pixel = delivery confirmation)
    ├─ CPC model     → settled to "spend" by the click event
    ├─ CPA model     → settled to "spend" by the conversion event
    └─ vCPM model    → settled to "spend" by the viewability event

Reservation that never settles (user navigated away, viewability never hit)
    → released after a timeout (configurable, default 24h)
    → makes "won-but-not-served" visible as a billable discrepancy for ops review
```

The migration steps:

1. **Add `auction_wins` ledger entry type** (`EntryAuctionWin` next to `EntrySpend`/`EntryReservation`) so the historical "spend on impression" entries remain valid while new "reservation on win → settlement on delivery" entries flow alongside. Don't co-mingle the schemas.
2. **`handleAuctionWin`** in reporting calls `billing.Engine.ProcessWin(event)` — the new method writes a reservation (not spend) sized at the clearing price, keyed by trace_id.
3. **`handleImpression`** in reporting switches its `ProcessEvent(EventType="impression")` call to a settlement lookup (`SettleReservation(traceID, "delivered")`). For CPM, settlement converts the reservation amount into a spend entry. For CPC/CPA/vCPM, settlement happens on click/conversion/view instead — the impression just records "delivered" without converting the reservation.
4. **Reservation expiry job** (`cmd/billing-expiry` — new) scans for reservations older than the configured TTL with no matching settlement, releases them, and emits `adtech.billing.reservation_released`.
5. **Update tests** — every spend test now operates on settled-spend, not raw spend. Delta assertions stay correct; the magnitudes change for CPC/CPA/vCPM cases (which currently skip).
6. **Remove the in-memory ledger fallback** and require Postgres ledger persistence to be in place first (per the "Persistence Strategy" build order above).

This migration is what closes the 8 skipped `tests/e2e/billing_models_test.go` cases (CPC/CPA/vCPM/CPCV reserve-settle, reservation expiry, tiered RS, etc.) and turns "auction.win as single source of truth for cost" from aspirational language into actual behavior.

Subjects from the table in "NATS Subjects (Async Events)" that have **no publisher yet** (placeholder for future work, harmless until something publishes them): `adtech.budget.depleted`, `adtech.campaign.state_changed`, `adtech.creative.review_completed`, `adtech.billing.reservation_*`, `adtech.privacy.opt_out`, `adtech.privacy.deletion_*`, all video/audio/dooh/retail/ingame/install subjects, `adtech.video.transcode_*`, `adtech.video.session_*`, `adtech.billing.tier_changed`, `adtech.audience.membership_updated`, `adtech.cleanroom.*`, `adtech.marketplace.*`, `adtech.pipeline.*`. These should be reviewed when their owning services come online.

#### What stays in-memory on purpose

Not everything needs to persist. Three categories are intentionally volatile:

- **Warm caches.** They reload from Postgres on demand. Persisting them would add complexity without buying anything — restart latency is already <100ms.
- **Dedup state.** A short post-restart window where one duplicate could slip through is acceptable; the alternative is making every tracker pixel hit a slower store.
- **Routing/shading learned state during early system bring-up.** Once stable traffic exists, snapshot (step 6 above). Until then, treat reset-on-restart as a feature — it's how A/B testing of new algorithms naturally clears prior bias.

### Database Schema (Core Entities - PostgreSQL)

Detailed schema in `migrations/`. See Campaign Hierarchy section for `insertion_orders`, `line_items`, `line_item_creatives` table definitions.

- **Account** - advertiser or publisher, org structure, roles, currency
- **InsertionOrder** - belongs to an advertiser, has total budget pool, flight dates, objective
- **LineItem** - belongs to an IO, has targeting, bid strategy, pacing, optional sub-budget. This is what "campaign" means throughout the plan.
- **Creative** - ad content (image, HTML, video, native), attached to line items with rotation weights
- **Targeting** - rules attached to a line item (inclusions + exclusions for geo, device, audience segments, domains, etc.)
- **Deal** - PMP, PG, or preferred deal between publisher and advertiser(s)
- **Publisher** - owns placements, has payment info, revenue share contract
- **Placement** - ad slot on a publisher's site/app (size, format, floor price, page URL pattern)
- **AudienceSegment** - named group of users for targeting
- **IdentityGraph** - cross-publisher, cross-device user links

**Note:** `campaign_id` in event schemas and Redis keys refers to the **line item ID**. The alias is maintained for simplicity - line items are the bidding entity.

### Event Schema (Analytics - DuckDB/ClickHouse)

Append-only event tables optimised for aggregation. All tables include `schema_version` and `insertion_order_id` for IO-level rollups.

- **impressions** - trace_id, insertion_order_id, campaign_id (line_item_id), creative_id, placement_id, publisher_id, geo, device, timestamp, cost
- **clicks** - trace_id, campaign_id, creative_id, placement_id, timestamp, landing_url
- **conversions** - trace_id, campaign_id, conversion_type, timestamp, revenue
- **auctions** - trace_id, placement_id, num_bids, winning_bid, clearing_price, timestamp, duration_ms

---

## Caching

### Layered Cache Architecture

The auction hot path has a ~100ms budget. Database round trips on every bid request are not viable. A three-layer cache keeps latency low while maintaining correctness.

```
Request comes in
    |
    v
L1: In-process cache (Go map)
    |  Hit? Return immediately. Zero network latency.
    |  Miss?
    v
L2: Redis (shared across instances)
    |  Hit? Return, populate L1.
    |  Miss?
    v
L3: Postgres (source of truth)
    |  Return, populate L2 and L1.
```

### What Lives Where

#### L1 - In-Process (Go maps, per-instance)

Data that is read-heavy and changes infrequently. Fastest possible access - no network hop.

| Service | Cached data | Invalidation |
|---|---|---|
| DSP | Active campaign configs, targeting rules | NATS: `adtech.cache.invalidate.campaigns` |
| Exchange | DSP endpoint list, DSP health/timeout stats | NATS: `adtech.cache.invalidate.dsp-endpoints` |
| Exchange | Floor prices per placement | NATS: `adtech.cache.invalidate.placements` |
| Ad Server | Creative metadata and content | NATS: `adtech.cache.invalidate.creatives` |
| SSP | Publisher placement configs | NATS: `adtech.cache.invalidate.placements` |
| Tracker | Signature validation keys | NATS: `adtech.cache.invalidate.keys` |

When a campaign, creative, or placement is updated via the dashboard, the gateway publishes an invalidation message to the relevant NATS subject. All instances of the affected service clear their L1 cache and reload on next access.

#### L2 - Redis (shared mutable state)

Data that needs atomic operations across multiple instances or fast shared read/write. All keys are prefixed by service name to avoid collisions.

| Service | Cached data | Redis key pattern | Redis operation | TTL |
|---|---|---|---|---|
| DSP | Budget balances per campaign | `dsp:budget:{campaign_id}` | `DECRBY` on win, `GET` on bid evaluation | No TTL, synced to Postgres periodically |
| DSP | Campaign config (L2 fallback) | `dsp:campaign:{campaign_id}` | `GET/SET` | 5 min |
| Ad Server | Frequency cap counters | `adserver:freqcap:{user}:{campaign}` | `INCR` with TTL | TTL matches cap window (e.g. 24h) |
| Ad Server | Creative metadata (L2 fallback) | `adserver:creative:{id}` | `GET/SET` | 10 min |
| Gateway | JWT session data, permissions | `gateway:session:{token}` | `GET/SET` | Matches token expiry |
| Reporting | Recent dashboard query results | `reporting:query:{hash}` | `GET/SET` | Short TTL (e.g. 60s) |

#### L3 - Postgres (source of truth)

All data originates from and is ultimately consistent with Postgres. Caches are always derived from L3, never the other way around.

### Budget Handling

Budget is the most sensitive cached value - overspend is real money lost. The approach:

1. Campaign budget loaded from Postgres into Redis on campaign start
2. On every auction win, Redis `DECRBY` atomically decrements the balance
3. DSP checks Redis balance before bidding - if zero, no-bid
4. A background goroutine periodically flushes Redis balance back to Postgres (e.g. every 10s)
5. If Redis goes down, DSP falls back to Postgres directly (slower but correct, no overspend)
6. When budget hits zero in Redis, DSP publishes `adtech.budget.depleted` via NATS

#### Pacing reconciliation (win-notice counter vs billed spend)

The Redis counter above is decremented on the **win notice** (nurl), which is fast and
overspend-safe but over-counts against actual billing in two ways: (a) a win that never
renders an impression still decrements pacing but never bills; (b) for CPC/CPA the full
clearing price is counted on the win, though spend only bills on the click/conversion. Left
alone this makes campaigns pace conservatively and under-deliver.

To close the gap without duplicating the billing state machine, the **billing engine is the
single source of truth** and the DSP mirrors it:

- `pkg/billing` maintains a per-campaign *committed* accumulator (`settled-today + open
  reserves`), fed from `billImmediate`/`reserve`/`settle` so it is backend-agnostic (works
  even for the TigerBeetle ledger, which doesn't persist `campaign_id`). Open reserves whose
  billable settle never arrives are swept after `reporting.pacing_hold_ttl` and released.
- `cmd/reporting` broadcasts the accumulator every `reporting.spend_snapshot_interval` on
  `adtech.billing.campaign_spend_snapshot`.
- Every DSP pod consumes it (fan-out) and `BudgetTracker.Reconcile` overwrites the Redis
  counter to the authoritative committed value. The local win-notice increment remains as the
  intra-snapshot overspend guard.

Net: between snapshots pacing is conservative (never overspends); on each snapshot it snaps
to billed reality (phantom wins released, CPC/CPA corrected). Reserve/settle/release lives in
`pkg/billing` where the rates are known — the DSP never re-computes spend. Bounded exposure: a
real win in the ~1s before a snapshot that hasn't yet impressed→reserved is dropped from the
committed figure until it does, so `spend_snapshot_interval` trades NATS traffic against how
tightly pacing tracks billing.

**Restart-safety + single-replica.** The committed accumulator is in-process, so reporting
persists the settled portion to `campaign_committed_spend` (mig 037) each snapshot tick and
re-hydrates it on boot (before event consumption) — otherwise a reporting restart would reset
committed to zero and reconcile every DSP counter down (overspend). Open reserves are not
persisted (transient; they rebuild from live events within the hold TTL). The snapshot
publisher is **single-replica by design** (reporting consumes on a shared queue group, so each
replica holds only a partial view); the base manifest pins `replicas: 1` (also required by
DuckDB's single-writer). Scaling reporting would require electing one publisher or deriving
committed from the shared analytics store.

### Cache Invalidation

Invalidation uses NATS pub/sub (not JetStream - fire-and-forget is fine here, worst case is serving stale data for one more request cycle):

```
Dashboard: update campaign targeting
    |
    v
Gateway --gRPC--> DSP (updates Postgres)
    |
    v
DSP publishes: adtech.cache.invalidate.campaigns {campaign_id: 123}
    |
    v
All DSP instances receive, clear campaign 123 from L1
Next bid request for campaign 123 loads fresh data from L2/L3
```

### Implementation

| Component | Location |
|---|---|
| L1 cache library | `pkg/cache/` - generic in-process cache with TTL and NATS invalidation listener |
| L2 Redis client | `pkg/cache/redis/` - Redis wrapper with typed get/set, atomic counters |
| Cache interface | `pkg/cache/cache.go` - interface so services don't know which layer they're hitting |

### Redis Topology

Same data, same code, same key prefixes - just different connection strings per environment via Kustomize overlays.

| Environment | Redis setup | Config |
|---|---|---|
| Local | 1 shared Redis | All services connect to `redis.default.svc:6379` |
| Staging | 1 shared Redis | All services connect to `redis.default.svc:6379` |
| Prod | Separate Redis per service | DSP connects to `redis-dsp.default.svc:6379`, Ad Server to `redis-adserver.default.svc:6379`, etc. |

Services read `REDIS_URL` from config - they don't know if it's shared or dedicated. Key prefixes are used in all environments (good practice even when isolated, and makes merging/splitting Redis instances trivial).

Locally it's one container in K8s. In prod, each service gets its own Redis manifest - independent scaling, failure isolation, no cross-service impact.

---

## Data Pipeline

### Overview

Publishers send data in all shapes and sizes - CSVs, TSVs, JSON, sometimes Excel. Field names vary, formats change without warning, columns get dropped or renamed. The data pipeline handles all of this: ingest anything, validate it, normalise it to a common schema, enrich it, and roll it up over time.

All pipeline data is stored as **Parquet files with Delta Log** (Delta Lake). This gives us columnar efficiency, ACID transactions, schema evolution, and time travel (rollback if a bad file corrupts things).

### Pipeline Stages

```
Ingest              Validate             Normalise            Enrich              Rollup
(accept anything)   (reject/flag bad)    (common schema)      (expand data)       (aggregate over time)

Publisher file  ->  Schema comparison -> Field mapping    ->  Geo lookup      ->  Minute/Hour/Day/Month
(CSV/TSV/JSON/      Row-level pass/fail  Type casting         Device classify     aggregations
 Excel/Parquet)     Quarantine failures  Date parsing         Audience matching   Retention policies
                    Drift detection      Dedup                User graph          Purge old granularity
                                         Null handling
```

### Stage Details

| Stage | What it does | Output location | Format |
|---|---|---|---|
| **Ingest** | Accept any format. Auto-detect delimiter, encoding, structure. Convert to Parquet. Keep original file untouched. | `landing/raw/` (originals), `landing/parquet/` (converted) | Raw Parquet |
| **Validate** | Compare against publisher config. Required fields present? Types correct? Values in range? Row-level pass/fail. | Good rows continue. Bad rows -> `quarantine/` with error details. | Validation report |
| **Normalise** | Map publisher-specific field names to common schema. Cast types. Parse dates. Handle nulls. Dedup. | `normalised/` | Parquet + Delta Log |
| **Enrich** | Expand with derived data - geo from IP, device classification from UA, audience segment matching, cross-publisher user connections. | `enriched/` | Parquet + Delta Log |
| **Rollup** | Aggregate into time-based summaries. Purge older granularities per retention policy. | `rollups/` | Parquet + Delta Log |

### Publisher Configuration

Each publisher gets a config file describing their data format and quirks:

```yaml
# profiles/publishers/acme_media.yaml
publisher_id: acme_media
format: csv
delimiter: ","
encoding: utf-8
date_format: "02/01/2006"
field_mapping:
  imp_cnt: impressions
  clks: clicks
  creative_name: creative_id
  geo: country
required_fields:
  - impressions
  - placement_id
  - date
validation_rules:
  impressions: ">=0"
  clicks: ">=0, <=impressions"
```

New publisher onboarded? Add a config file. No code changes.

### Schema Drift Detection

Publisher data is unstable. The pipeline detects and handles schema changes automatically:

| Scenario | Detection | Action |
|---|---|---|
| New column appears | Column not in field_mapping | Pass through as `_unmapped_{name}`, alert |
| Expected column missing (required) | Required field absent | Quarantine file, alert |
| Expected column missing (optional) | Optional field absent | Fill with null, warn |
| Column type changes | Type coercion fails | Quarantine affected rows, alert |
| Column renamed | Old field gone, new unknown field appears | Fuzzy match suggests mapping, quarantine until confirmed |
| Delimiter/format changes | Was CSV, now TSV | Auto-detect on every file |
| Entirely new schema | Most fields unrecognised | Quarantine entire file, alert, needs manual config update |

**Principle: never silently drop data, never silently produce wrong data.** Either process it correctly or quarantine it and tell someone.

**Drift dashboard** in the UI:
- Schema change history per publisher
- Current vs expected schema diff
- Quarantine queue with suggested fixes
- Accept/reject buttons - account manager confirms new mapping, config updates, quarantined files reprocess automatically

### Data Rollups

Raw event-level data is expensive to store and slow to query. The pipeline continuously rolls data up into coarser granularities and purges older fine-grained data.

**Rollup chain:**

```
Raw events (every impression/click)
  |  retained 24-48 hours
  v
Minute rollups
  |  retained 7 days
  v
Hourly rollups
  |  retained 90 days
  v
Daily rollups
  |  retained 2 years
  v
Monthly rollups
  |  retained forever
```

**What gets rolled up:**

| Granularity | Dimensions (group by) | Metrics (sum/avg) | Retention |
|---|---|---|---|
| Raw | trace_id, every field | individual event | 24-48 hours |
| Minute | campaign, creative, placement, geo, device | impressions, clicks, conversions, spend | 7 days |
| Hourly | campaign, creative, placement, geo, device | impressions, clicks, conversions, spend, avg_bid, win_rate | 90 days |
| Daily | campaign, creative, placement, geo, device | impressions, clicks, conversions, spend, avg_bid, win_rate, CTR, eCPM | 2 years |
| Monthly | campaign, creative, placement, geo | impressions, clicks, conversions, spend, CTR, eCPM, ROAS | Forever |

**Rollup jobs run as K8s CronJobs:**

| Job | Schedule | Action |
|---|---|---|
| `rollup-minute` | Every minute | Aggregate raw events -> minute rollups |
| `rollup-hourly` | Every hour | Aggregate minute rollups -> hourly rollups |
| `rollup-daily` | Every day at 00:30 UTC | Aggregate hourly -> daily. Purge raw events > 48h. Purge minute rollups > 7d. |
| `rollup-monthly` | 1st of month at 01:00 UTC | Aggregate daily -> monthly. Purge hourly rollups > 90d. |

**Reporting auto-selects the right tier** based on query time range:

| Dashboard query | Reads from |
|---|---|
| Last 30 minutes | Minute rollups |
| Today | Hourly rollups |
| Last 7 days | Daily rollups |
| Last quarter | Monthly rollups |
| Trace this request | Raw events (if within retention window) |

### Universal Rollup Framework

The rollup pattern above applies to events, but all high-volume data sources need the same treatment. A generic rollup engine in `pkg/store/rollup/` handles all of them via configuration:

| Data source | Volume | Dimensions | Metrics | Same schedule |
|---|---|---|---|---|
| **Events** (impressions, clicks, conversions, views) | Highest | campaign, creative, placement, geo, device | count, spend, CTR, eCPM | Yes |
| **Auction logs** | Very high | placement, dsp, geo, deal_type | auction_count, avg_clearing_price, fill_rate, timeout_rate, avg_bid_count | Yes |
| **Bid request/response logs** | Very high | dsp, placement, geo | bid_count, avg_bid_price, avg_response_time_ms, no_bid_rate | Yes |
| **Fraud scoring logs** | High | placement, publisher, fraud_type | event_count, avg_fraud_score, block_rate | Yes |
| **Budget transactions** | Medium | campaign, transaction_type (reserve/settle/release) | count, total_amount | Hourly -> daily -> monthly |
| **Publisher ingested data** | Varies | publisher, file_type, validation_status | file_count, row_count, quarantine_rate | Daily -> monthly |

Each data source is defined as a rollup config:

```go
RollupConfig{
    Source:     "auctions",
    Dimensions: []string{"placement_id", "dsp_id", "geo"},
    Metrics:    []string{"count", "avg_clearing_price", "fill_rate", "timeout_rate"},
    Schedule:   StandardSchedule,  // minute -> hourly -> daily -> monthly
    Retention:  StandardRetention, // 48h -> 7d -> 90d -> 2y -> forever
}
```

One rollup engine, many configurations. `cmd/rollup/` reads all configs and runs the appropriate aggregation per schedule.

### Implementation

| Component | Location |
|---|---|
| Pipeline service | `cmd/pipeline/` - long-running service for ingest, validate, normalise, enrich |
| Rollup jobs | `cmd/rollup/` - short-lived Go binary, runs as K8s CronJob |
| Rollup engine | `pkg/store/rollup/` - generic rollup framework, config-driven |
| Pipeline logic | `pkg/pipeline/` - format detection, validation, normalisation, enrichment |
| Publisher configs | `profiles/publishers/` - per-publisher YAML configs |
| Delta/Parquet handling | `pkg/store/datalake/` - read/write Parquet, Delta Log management |

---

## Optimisation Pipelines

### Overview

These are the "make more money" and "achieve campaign goals" engines. They consume historical data from the analytics store, detect patterns, and feed adjustments back into the live system.

### Pipelines

| Pipeline | What it does | Input | Output |
|---|---|---|---|
| **Bid optimisation** | Analyse win/loss data to bid smarter. Detect overpaying (winning at $2 when second bid is $0.50). | Auction history (win price, clearing price, bid count) | Adjusted bid prices per campaign/placement |
| **Placement scoring** | Rank placements by performance for each campaign/vertical | Impression, click, conversion data per placement | Placement scores used by DSP bid logic |
| **Audience discovery** | Find patterns in conversion data to create new segments | Conversion events + user attributes | New audience segments in `pkg/models` |
| **Creative performance** | Identify which creatives perform best, auto-rotate towards winners | CTR, conversion rate per creative per campaign | Creative weight adjustments (serve winner more) |
| **Budget reallocation** | Shift spend towards what's working (geo, device, placement) | Spend + performance data by dimension | Budget split adjustments per campaign |
| **Publisher yield** | Help publishers maximise revenue by optimising floor prices and fill rates | Auction data per placement (fill rate, avg clearing price, floor price) | Floor price recommendations |

### How They Feed Back

```
Analytics Store (DuckDB/ClickHouse)
    |
    v
Optimisation Pipeline (reads historical data, runs analysis)
    |
    +---> DSP (adjust bid strategy, budget splits) via gRPC
    +---> Exchange (update placement scores) via NATS cache invalidation
    +---> Reporting (surface recommendations in dashboard) via Postgres
```

Adjustments can be:
- **Automated** - pipeline directly updates bid parameters within configured bounds (e.g. bid price can auto-adjust +/- 20%)
- **Recommended** - pipeline writes a recommendation, human reviews and approves in dashboard

### Execution

| Type | How | Schedule |
|---|---|---|
| Scheduled analysis | K8s CronJobs (`cmd/optimise/`) | Hourly or daily depending on pipeline |
| Continuous scoring | Long-running service consuming from NATS | Real-time as events arrive |

### Where Python Fits

Some optimisation work (ML model training, statistical analysis, clustering for audience discovery) is better suited to Python. The approach:

- **Training** happens offline in Python (Jupyter notebooks, scikit-learn, etc.) in a `python/` directory
- **Models export** as simple rules, scores, or lookup tables that the Go services consume
- **Inference** happens in Go using the exported model artifacts - no Python in the hot path
- **Heavy analysis** (e.g. audience clustering) can run as Python K8s CronJobs that write results to Postgres/analytics store

---

## Fraud Detection and Traffic Quality

### Overview

Fraud protection is essential - advertisers don't pay for fake traffic, publishers don't get penalised for fraud they didn't commit. Detection happens at two speeds.

### Real-Time Detection (Hot Path)

Basic checks at the tracker and exchange level before counting an event. Must be fast - adds minimal latency to the request.

| Check | Where | How | Action |
|---|---|---|---|
| Bot user agent | Tracker | Match against known bot UA list (IAB bots list) | Reject, don't record event |
| Data centre IPs | Tracker | IP range blocklist (AWS, GCP, Azure, known proxy ranges) | Flag as suspicious, don't bill |
| Rate per user | Tracker | Redis counter per user/IP - too many events in short window | Flag, rate limit |
| ads.txt validation | Exchange | Verify publisher is authorised to sell inventory | Reject bid request |
| Request signature | Tracker | Validate HMAC signature on pixel URLs | Reject tampered events |
| Basic viewability | Ad Server | Ad must be in viewport, minimum size, minimum time | Mark as non-viewable, don't bill |

Implemented in `pkg/fraud/realtime.go`. Runs as middleware in the tracker and exchange.

### Batch Detection (Cold Path)

Deeper analysis on historical data to find patterns humans and simple rules would miss.

| Detection | What it finds | How |
|---|---|---|
| Click fraud patterns | Same user clicking same ad repeatedly, click farms with coordinated timing | Statistical analysis on click events - abnormal frequency, timing patterns |
| Impression stacking | Multiple ads rendered in same pixel space | Cross-reference impression events with placement dimensions |
| Conversion fraud | Fake conversion postbacks, inflated numbers | Validate conversion timing (click-to-conversion window), source verification |
| Device fingerprint anomalies | Same "user" with impossible device/IP combinations | Cluster analysis on device signals |
| Publisher quality scoring | Publishers with consistently suspicious traffic patterns | Aggregate fraud signals per publisher over time |
| Coordinated fraud rings | Multiple publishers or users acting in concert | Graph analysis on user-publisher-IP relationships |

### Fraud Scoring

Every event gets a **fraud score** (0.0 = definitely legitimate, 1.0 = definitely fraud):

```
Event arrives at Tracker
    |
    v
Real-time checks (bot UA, IP blocklist, rate limit)
    |
    +-- Hard block? --> Reject immediately, log reason
    |
    +-- Pass? --> Assign initial fraud_score based on real-time signals
                  Record event with fraud_score
                  |
                  v
             Batch analysis (runs later)
                  |
                  v
             Update fraud_score on historical events
             Flag events above threshold
             Exclude from billing
```

The `fraud_score` is a column on every event table (impressions, clicks, conversions). Reporting queries can filter by score threshold. Billing only counts events below a configurable threshold.

### Billing Impact

| Fraud score | Treatment |
|---|---|
| 0.0 - 0.3 | Clean - billed normally |
| 0.3 - 0.7 | Suspicious - flagged for review, billed but disputable |
| 0.7 - 1.0 | Fraudulent - excluded from billing, credited back to advertiser |

### Dashboard

Fraud detection feeds into the reporting dashboard:
- Traffic quality overview per publisher
- Fraud rate trends over time
- Flagged events with drill-down to reason
- Publisher quality scores
- Advertiser-facing "invalid traffic" report (industry standard - IVT reporting)

### Implementation

| Component | Location |
|---|---|
| Real-time fraud checks | `pkg/fraud/realtime.go` - middleware for tracker and exchange |
| Batch fraud analysis | `cmd/fraud/` - K8s CronJob, reads from analytics store |
| Fraud scoring logic | `pkg/fraud/scoring.go` - combines signals into fraud_score |
| Bot/IP blocklists | `pkg/fraud/lists/` - updated periodically, loaded into L1 cache |
| ML model artifacts | `python/fraud/` - training scripts, exported models |
| Fraud rules config | `profiles/fraud/rules.yaml` - configurable thresholds and weights |

### IAB Compliance

Follow IAB Tech Lab standards where applicable:
- **ads.txt / sellers.json** - authorised seller verification
- **IVT Guidelines** - General Invalid Traffic (GIVT) and Sophisticated Invalid Traffic (SIVT) classification
- **Open Measurement SDK** - viewability measurement standards (deferred, but designed for)

---

## ads.txt and sellers.json

### What They Are

**ads.txt** is a text file publishers place at their domain root (`example.com/ads.txt`) listing who is authorised to sell their inventory. It prevents domain spoofing - someone pretending to sell premium publisher inventory when they're not.

**sellers.json** is the sell-side equivalent - our platform publishes a `sellers.json` at our domain listing all publishers we represent.

### ads.txt Verification

The Exchange validates ads.txt before running an auction:

```
Bid request arrives claiming to be from "nytimes.com"
    |
    v
Exchange checks ads.txt cache:
    Is our platform listed as an authorised seller for nytimes.com?
    |
    +-- Yes -> Proceed with auction
    +-- No  -> Reject bid request (domain spoofing)
    +-- No ads.txt file found -> Flag as unverified, proceed with lower trust
```

### ads.txt Crawler

A CronJob that periodically fetches and caches ads.txt files for all registered publishers:

| Concern | Approach |
|---|---|
| Crawl schedule | Every 24 hours per publisher |
| Storage | Postgres `ads_txt_cache` table (domain, entries, last_fetched, status) |
| L1 cache | Exchange loads into in-process cache on startup, refreshed via NATS invalidation |
| New publisher | Crawled immediately on registration |
| File missing | Publisher flagged in dashboard, bid requests from that domain marked as unverified |
| File changed | Diff detected, dashboard alert, cache updated |
| Timeout/error | Retry with backoff, keep previous version, alert after 3 failures |

### sellers.json

Our platform publishes its own `sellers.json` at our domain, listing all registered publishers:

```json
{
    "sellers": [
        {
            "seller_id": "pub_123",
            "name": "Acme Media",
            "domain": "acmemedia.com",
            "seller_type": "PUBLISHER"
        }
    ]
}
```

Auto-generated from the publisher database. Served by the Gateway at `/sellers.json`. Updated when publishers are added/removed.

### Implementation

| Component | Location |
|---|---|
| ads.txt crawler | `cmd/adstxt/` - K8s CronJob, fetches and caches ads.txt files |
| ads.txt verification | `pkg/fraud/adstxt.go` - lookup during auction, loaded into L1 cache |
| sellers.json endpoint | Gateway serves at `/sellers.json`, auto-generated from Postgres |
| Crawler config | Live config: `adstxt.crawl_interval_hours`, `adstxt.timeout_seconds` |

---

## Email

### Approach

`pkg/email/` with a send interface - services don't know which backend they're using. Kustomize overlay selects the provider per environment.

| Environment | Provider | Behaviour |
|---|---|---|
| Local | Mailpit (fake SMTP with web UI) | All emails caught in `http://mailpit.localhost` - nothing leaves the machine |
| Staging | Mailpit or SES sandbox | Test delivery without spamming real users |
| Prod | AWS SES, Sendgrid, or any SMTP provider | Real delivery |

### What Sends Email

| Trigger | Email | Recipient |
|---|---|---|
| Registration | "Verify your email address" | New user |
| Password reset | "Click here to reset your password" | User |
| Team invite | "You've been invited to join X's account" | Invited user |
| Campaign approved | "Your campaign is now live" | Advertiser |
| Creative rejected | "Your creative was rejected: [reason]" | Advertiser |
| Invoice ready | "Your invoice for [period] is available" | Advertiser billing contact |
| Payout processed | "Your payout of $X has been sent" | Publisher payout contact |
| Budget alert | "Campaign X budget is 90% depleted" | Advertiser |
| Fraud alert | "Unusual traffic detected on placement X" | Publisher |
| Scheduled report | "Your [weekly/monthly] report is attached" | Subscriber (CSV/PDF attached) |

### Email Templates

Go `html/template` files in `web/templates/email/`. Same template engine as the dashboard - no extra tooling.

### Implementation

| Component | Location |
|---|---|
| Email interface + templates | `pkg/email/` - `Send()` function, template rendering |
| Mailpit deployment | `k8s/base/mailpit/` - local/staging SMTP + web UI |
| SMTP config | Kustomize overlays - connection string, credentials per environment |
| Notification preferences | Postgres - per-account settings for which emails to receive |

---

## Ingress (Traefik)

### Why Traefik

Built into k3s - already installed, zero setup. Same local and prod. Handles TLS termination, path-based routing, and has a built-in dashboard to see all routes.

### Routing

```
Internet -> Traefik (TLS termination)
    |
    +-> /v1/api/*        -> Gateway (dashboard, REST API)
    +-> /v1/openrtb/*    -> Exchange (bidding - direct for performance)
    +-> /v1/t/*          -> Tracker (pixels - direct, highest volume, most latency-sensitive)
    +-> /docs            -> Gateway (Swagger UI)
```

Tracker and Exchange endpoints bypass the Gateway and route directly to their services via Traefik. These are the hot path - adding an extra hop through the Gateway wastes latency for high-volume, time-critical requests.

### TLS

| Environment | TLS |
|---|---|
| Local | Self-signed cert (Traefik auto-generates) or plain HTTP |
| Staging | Let's Encrypt via Traefik's built-in ACME |
| Prod | Let's Encrypt or managed certificate |

### Traefik Dashboard

Traefik includes a built-in dashboard showing all routes, services, and middleware. Available locally at `http://traefik.localhost` - another visibility tool for devs.

### Implementation

| Component | Location |
|---|---|
| Ingress routes | `k8s/base/ingress/` - Traefik IngressRoute manifests |
| TLS config | `k8s/overlays/{env}/` - per-environment TLS settings |

---

## Autoscaling (HPA)

K8s Horizontal Pod Autoscaler automatically adds/removes pod replicas based on load.

### Per-Service Autoscaling

| Service | Scale on | Local (min/max) | Staging (min/max) | Prod (min/max) |
|---|---|---|---|---|
| Tracker | CPU / req per sec | 1 / 4 | 2 / 10 | 2 / 50 |
| Exchange | CPU / req per sec | 1 / 4 | 2 / 10 | 2 / 30 |
| Gateway | CPU / req per sec | 1 / 3 | 2 / 5 | 2 / 20 |
| Ad Server | CPU / req per sec | 1 / 3 | 2 / 5 | 2 / 20 |
| DSP | CPU | 1 / 3 | 2 / 5 | 2 / 20 |
| SSP | CPU | 1 / 3 | 2 / 5 | 2 / 10 |
| Reporting | CPU / memory | 1 / 2 | 1 / 3 | 2 / 10 |
| Webhooks | CPU | 1 / 2 | 1 / 3 | 2 / 10 |

**Not autoscaled:** CronJobs (rollup, fraud, billing), Pipeline (concurrency-limited), infra (Postgres, NATS, Redis - scaled via replicas, not HPA).

**Note:** Reporting with DuckDB is single-writer, so max 1 when using DuckDB. HPA only applies to reporting when using ClickHouse backend.

### How It Works

```
Normal:     Tracker [pod] [pod]                              100 req/sec
Spike:      Tracker [pod] [pod] [pod] [pod] [pod] [pod]     1000 req/sec (HPA scaled up)
After:      Tracker [pod] [pod]                              100 req/sec (HPA scaled down)
```

HPA checks metrics every 15 seconds. Scale-up is fast (seconds). Scale-down is slow (5 min cooldown) to avoid flapping.

### Testing Locally

The `stress` seed + `burst` simulation profile triggers autoscaling locally:

1. `tilt up` - services start at min replicas
2. Run `burst` simulation - traffic spikes
3. Watch HPA scale up pods in Tilt dashboard or `kubectl get hpa`
4. Stop simulation - pods scale back down after cooldown

This validates autoscaling behaviour using the same test profiles we already have.

### Implementation

HPA manifests live alongside each service's deployment:
- `k8s/base/{service}/hpa.yaml` - base HPA config (target CPU %, metric thresholds)
- `k8s/overlays/local/` - patches min/max to local values
- `k8s/overlays/staging/` - patches to staging values
- `k8s/overlays/prod/` - patches to prod values

---

## Health Checks

Every service exposes two HTTP endpoints for K8s probes:

| Endpoint | K8s Probe | What it checks | Returns |
|---|---|---|---|
| `/healthz` | Liveness | Is the process alive and not deadlocked? | `200` if alive, `503` if stuck |
| `/readyz` | Readiness | Is the service ready to accept traffic? | `200` if ready, `503` if not |

### Readiness Checks per Service

| Service | `/readyz` passes when |
|---|---|
| DSP | Connected to Postgres, Redis, and NATS |
| SSP | Connected to Postgres |
| Exchange | Connected to NATS, at least one DSP reachable |
| Ad Server | Connected to object storage, Redis |
| Tracker | Connected to NATS, Redis |
| Reporting | Connected to NATS, analytics store (DuckDB file accessible or ClickHouse reachable) |
| Gateway | Connected to at least one downstream gRPC service |
| Pipeline | Connected to Postgres, object storage |
| Billing | Connected to Postgres, NATS |
| Webhooks | Connected to NATS |

K8s uses readiness to decide whether to route traffic to a pod. A pod that fails readiness is removed from the service endpoint - no traffic until it recovers. This prevents requests hitting a pod that lost its database connection.

Liveness is simpler - if the process is running and the health endpoint responds, it's alive. If it deadlocks and stops responding, K8s restarts the pod.

### Implementation

Health check logic lives in `pkg/health/` - a shared library that registers dependency checks and exposes the two endpoints. Each service registers its specific dependencies at startup.

---

## Rate Limiting and Back-Pressure

### Inbound Rate Limiting (External-Facing)

Internet-facing endpoints need protection from floods, bot traffic, and abuse.

| Endpoint | Strategy | Limit example |
|---|---|---|
| Tracker (`/v1/t/imp`, `/v1/t/click`, `/v1/t/conv`) | Rate limit per IP + per placement_id | 1000 req/sec per placement, 100 req/sec per IP |
| SSP bid requests | Rate limit per publisher | 500 req/sec per publisher |
| Gateway API | Rate limit per account + per API key | 100 req/sec per API key |

Returns `429 Too Many Requests` when exceeded. Implemented in `pkg/middleware/ratelimit.go` using Redis sliding window counters.

### Internal Back-Pressure

| Point | Strategy |
|---|---|
| Exchange -> DSP | Handled by timeout. DSP has 100ms to respond. Overloaded = no response = no-bid. No explicit rate limit needed. |
| Tracker -> NATS | JetStream flow control. If stream hits max bytes/messages, publisher gets back-pressure signal. Tracker buffers briefly in-memory or drops with a metric. |
| NATS -> Reporting | JetStream `MaxAckPending` limits in-flight unacked messages. Prevents unbounded memory. Messages queue in the stream until reporting catches up. |
| Pipeline file processing | Concurrency limit - max N files processed in parallel. New uploads queue. Dashboard shows queue depth. |

### Circuit Breakers

When a downstream service is failing, stop hammering it:

| Caller | Target | Behaviour |
|---|---|---|
| Exchange | DSP | DSP times out X times in a row -> circuit opens, skip that DSP for N seconds |
| DSP | Redis (budget) | Redis down -> fall back to Postgres, circuit breaker prevents repeated connection attempts |
| Ad Server | Object storage | S3/filesystem slow -> serve cached creative or default ad |
| Any service | Postgres | Postgres slow -> circuit opens, return errors rather than queueing and cascading |

Circuit breaker state tracked in L1 cache (per-instance), exposed as Prometheus metrics.

### Load Shedding

When a service is overwhelmed beyond what rate limiting can handle:

| Service | Strategy |
|---|---|
| Tracker | Prioritise by event type: impressions > clicks > conversions (impressions are the billing event). If shedding, drop conversions first. |
| Exchange | Reduce DSP fan-out - only call top N performing DSPs instead of all, reducing outbound requests. |
| Pipeline | Stop accepting new file uploads until queue drains below threshold. Return `503 Service Unavailable` with `Retry-After` header. |

### Smart Routing (Exchange DSP Fan-Out)

Generic load balancers don't understand ad tech. The exchange's DSP selection logic evolves over time from simple to smart:

**Phase 1 (MVP):** Broadcast to all registered DSPs. K8s service DNS for routing. Simple and correct.

**Phase 2:** Basic filtering before fan-out:
- Skip DSPs with no active campaigns
- Skip DSPs with fully depleted budgets
- Skip DSPs whose campaigns don't match the request's geo/device at all

**Phase 3:** Smart weighted routing:
- **Performance-weighted** - send more traffic to DSPs with higher win rates and faster response times
- **Budget-aware** - reduce traffic to DSPs with low remaining budget (they'll no-bid most requests anyway)
- **Latency-aware** - if a DSP's p99 is creeping up, reduce its share before it starts timing out
- **Targeting-aware** - don't send a UK-only request to a DSP that only has US campaigns

This logic lives in `pkg/auction/router.go`. Each phase builds on the previous one. The exchange tracks DSP performance metrics (win rate, avg response time, timeout rate) in L1 cache and uses them for routing decisions.

### Monitoring

All rate limits, circuit breaker states, queue depths, and shed events are exposed as Prometheus metrics:

- `tracker_requests_rate_limited_total`
- `exchange_circuit_breaker_state{dsp="..."}`
- `exchange_dsp_fanout_count` / `exchange_dsp_skip_reason{reason="budget_depleted"}`
- `nats_stream_pending_messages{stream="EVENTS"}`
- `pipeline_queue_depth`
- `service_load_shed_total{service="...", type="..."}`

Visible in Grafana - pressure building is visible before things break.

### Implementation

| Component | Location |
|---|---|
| Rate limiting middleware | `pkg/middleware/ratelimit.go` - HTTP middleware, Redis-backed counters |
| Circuit breaker | `pkg/middleware/circuitbreaker.go` - wraps any outbound call |
| Smart DSP router | `pkg/auction/router.go` - DSP selection and fan-out logic |

---

## Deployments and Graceful Shutdown

### Deployment Strategies

#### Rolling Update (Default)

Standard K8s rolling update. New pods come up, old pods drain. Zero downtime.

```
v1 v1 v1  ->  v1 v1 v2  ->  v1 v2 v2  ->  v2 v2 v2
```

Used for most services and routine deploys. No extra configuration needed.

#### Canary Deploy

Route a small percentage of traffic to the new version. Monitor metrics. Increase if good, roll back instantly if bad.

```
v1 v1 v1 v1 v1    (100% to v1)
v1 v1 v1 v1 v2    (20% to v2 - monitor for 10 min)
v1 v1 v2 v2 v2    (60% to v2 - metrics look good)
v2 v2 v2 v2 v2    (100% to v2 - promoted)
```

Implemented as two K8s Deployments (stable + canary) with traffic split controlled by replica count or application-level routing.

#### A/B Deploy (Multi-Version Testing)

Run two versions simultaneously and compare performance. Critical for ad tech where algorithm changes directly affect revenue.

| Version | Traffic split | Measuring |
|---|---|---|
| DSP v1 (current bid algorithm) | 50% | Win rate, eCPM, ROAS, spend efficiency |
| DSP v2 (new bid algorithm) | 50% | Same metrics - compare against v1 |

What can be A/B tested:

- Bid algorithms and strategies
- Pacing logic
- Targeting evaluation
- Fraud scoring models
- Creative rotation strategies
- Floor price optimisation

### A/B Testing Implementation

The Exchange already has smart routing in `pkg/auction/router.go`. Extend it to support version-aware routing:

```
Exchange receives bid request
    |
    v
Router checks: is there an active A/B test?
    |
    +-- Yes --> Route to v1 or v2 based on configured split %
    |           Tag trace with version for metrics comparison
    |
    +-- No  --> Normal routing
```

A/B test configuration lives in the live config store (Postgres config table, editable from dashboard):

```json
{
    "key": "exchange.ab_test",
    "value": {
        "enabled": true,
        "versions": {"v1": "dsp-stable", "v2": "dsp-canary"},
        "split": {"v1": 50, "v2": 50},
        "metrics": ["win_rate", "ecpm", "roas", "latency_p99"],
        "started_at": "2026-05-27T10:00:00Z"
    }
}
```

Both versions run as separate K8s Deployments with different image tags. The Exchange routes to them by service name. Prometheus tracks all metrics tagged by version.

### Graceful Shutdown

Every service must shut down cleanly during deploys. K8s sends SIGTERM, service has `terminationGracePeriodSeconds` (e.g. 30s) to drain before being killed.

| Service | On shutdown must... | How |
|---|---|---|
| Exchange | Finish in-flight auctions, stop accepting new ones | `preStop` hook + `signal.NotifyContext`, drain HTTP/gRPC server |
| DSP | Finish in-flight bid evaluations, flush budget to Postgres | Drain, background goroutine flushes Redis -> Postgres |
| Tracker | Flush buffered events to NATS before exit | Drain internal buffer, confirm NATS publish acks |
| Reporting | Ack pending NATS messages, flush writes to analytics store | Stop consuming, finish pending writes, ack all |
| Ad Server | Finish serving in-flight creatives | Standard HTTP graceful shutdown |
| Pipeline | Finish current file processing, don't start new ones | Check shutdown flag between pipeline stages |
| Gateway | Drain HTTP connections, finish in-flight API calls | `preStop` sleep (let load balancer remove pod) + graceful drain |

Implementation pattern (same for every service):

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
defer stop()

// Start server
go server.Serve(listener)

// Wait for shutdown signal
<-ctx.Done()

// Drain (service-specific logic)
shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
defer cancel()
server.Shutdown(shutdownCtx)
flushBuffers()
```

Shared shutdown logic lives in `pkg/lifecycle/` so every service follows the same pattern.

### Deployment Monitoring

Grafana dashboard shows v1 vs v2 metrics side by side during canary and A/B deploys:

| Metric | Source | Alert if |
|---|---|---|
| Error rate by version | Prometheus: `request_errors_total{version="..."}` | v2 error rate > v1 by more than 1% |
| Latency by version | Prometheus: `request_duration_seconds{version="..."}` | v2 p99 > v1 p99 by more than 20% |
| Win rate by version | Prometheus: `auction_wins_total{version="..."}` | v2 win rate drops significantly |
| Event loss during deploy | NATS: pending message count | Pending messages growing during rollout |
| Budget accuracy | Redis balance vs Postgres balance | Drift > threshold during rollout |

### Automated Rollback

If canary metrics breach alert thresholds:

1. Grafana alert fires
2. CI/CD webhook receives alert
3. Canary deployment scaled to 0 automatically
4. All traffic returns to stable version
5. Team notified via alert channel

For A/B tests, no auto-rollback - the test runs for a configured duration and the team reviews results in the dashboard before promoting.

### Implementation

| Component | Location |
|---|---|
| Graceful shutdown | `pkg/lifecycle/` - shared shutdown, drain, flush logic |
| Version-aware routing | `pkg/auction/router.go` - A/B split logic in exchange router |
| A/B test config | Live config store (Postgres config table, dashboard editable) |
| Canary manifests | `k8s/base/{service}/deployment-canary.yaml` |
| Deployment dashboard | Grafana: pre-built v1-vs-v2 comparison dashboard |
| Operations UI | Gateway dashboard - Operations section |

### Operations UI (Gateway Dashboard)

An operations section in the HTMX dashboard for managing deployments and experiments.

**A/B Test Management:**
- Create new A/B test - pick service, set traffic split %, select metrics to compare
- View active tests with live metrics side by side (v1 vs v2)
- Adjust split % while test is running
- End test - promote winner or revert

**Canary Management:**
- Current canary status per service (version, traffic %, health)
- Promote canary to stable (one click)
- Rollback canary (one click)
- Canary history - previous canary deploys with outcome

**Deployment Overview:**
- Current version running per service (image tag / git SHA)
- When each service was last deployed and by who
- Deployment timeline - visual history of all deploys
- Link to git commit for each running version

**Live Metrics Comparison:**
- Side-by-side panels for any two versions: error rate, latency, win rate, throughput
- Pulled from Prometheus, rendered as embedded Grafana panels or native charts
- Auto-refreshing during active experiments

**Gateway API endpoints for operations:**
- `GET    /v1/api/ops/deployments` - current version per service
- `GET    /v1/api/ops/ab-tests` - list active and past A/B tests
- `POST   /v1/api/ops/ab-tests` - create A/B test
- `PUT    /v1/api/ops/ab-tests/{id}` - update split %, end test
- `GET    /v1/api/ops/ab-tests/{id}/metrics` - live comparison metrics
- `POST   /v1/api/ops/canary/{service}/promote` - promote canary
- `POST   /v1/api/ops/canary/{service}/rollback` - rollback canary

All operations actions go through the audit log.

### Local Canary and A/B Testing Workflow

Developers test canary deploys and A/B tests locally before they hit staging or prod. Same infrastructure, same tools, same workflow - just on their laptop.

#### How It Works Locally

Tilt manages both the **stable** and **canary** versions of a service simultaneously:

```
Developer is working on a new DSP bid algorithm:
    |
    v
Step 1: Current code is running as "stable" (what's in main)
    Tilt has built and deployed dsp:stable from latest main

Step 2: Developer makes changes to pkg/auction/shading.go
    |
    v
Step 3: Click "Deploy Canary" button in Tilt for DSP
    Tilt builds a second image from current working directory: dsp:canary
    Tilt deploys a second DSP Deployment alongside the stable one
    |
    v
Step 4: Two DSP pods now running:
    dsp-stable (from main) - handles 80% of traffic
    dsp-canary (developer's changes) - handles 20% of traffic
    |
    v
Step 5: Run simulation: trickle or steady profile
    Exchange routes bid requests to both DSPs based on split %
    |
    v
Step 6: Open Grafana or Trace Explorer:
    Compare v1 vs v2 side by side:
    - Win rate: stable 45% vs canary 52% ✓
    - Avg clearing price: stable $2.50 vs canary $2.20 ✓ (shading working better)
    - Latency: stable 12ms vs canary 14ms (slightly slower, acceptable)
    - Error rate: stable 0% vs canary 0% ✓
    |
    v
Step 7: Looks good -> Click "Promote" in Tilt
    Tilt replaces stable with canary code
    Canary deployment removed
    Back to single DSP pod
    |
    v
Step 8: Commit, push, PR
    CI runs the same test with automated pass/fail criteria
```

#### Tilt Buttons for Canary/A/B

```python
# Tiltfile additions for canary support

# For each service, add canary buttons
canary_services = ['dsp', 'exchange', 'adserver', 'tracker', 'reporting']

for svc in canary_services:
    # Button: Deploy Canary
    local_resource(
        svc + '-canary-deploy',
        cmd='kubectl apply -f k8s/base/' + svc + '/deployment-canary.yaml',
        trigger_mode=TRIGGER_MODE_MANUAL,
        labels=['canary']
    )

    # Button: Promote Canary
    local_resource(
        svc + '-canary-promote',
        cmd='kubectl delete -f k8s/base/' + svc + '/deployment-canary.yaml',
        trigger_mode=TRIGGER_MODE_MANUAL,
        labels=['canary']
    )

    # Button: Rollback Canary
    local_resource(
        svc + '-canary-rollback',
        cmd='kubectl delete -f k8s/base/' + svc + '/deployment-canary.yaml',
        trigger_mode=TRIGGER_MODE_MANUAL,
        labels=['canary']
    )
```

The Tilt dashboard shows canary buttons alongside the normal service status:

```
┌─ Tilt Dashboard ──────────────────────────────────────────┐
│                                                            │
│  Services:                                                 │
│  ✓ dsp (stable)        [Deploy Canary] [Logs]             │
│  ✓ dsp-canary (20%)   [Promote] [Rollback] [Logs]        │
│  ✓ exchange            [Deploy Canary] [Logs]             │
│  ✓ tracker             [Logs]                              │
│  ✓ reporting           [Logs]                              │
│  ...                                                       │
│                                                            │
│  Canary Status:                                            │
│  DSP: canary active (20% traffic)                         │
│  Metrics: win_rate +7%, latency +2ms, errors 0%           │
│                                                            │
└────────────────────────────────────────────────────────────┘
```

#### A/B Testing Locally

For A/B tests (50/50 split, longer running), the same workflow applies but with equal traffic split:

```
Step 1: Click "Create A/B Test" in Operations UI (localhost)
    Select: service=DSP, split=50/50, metrics=[win_rate, ecpm, latency]
    |
    v
Step 2: Operations UI writes to live config store:
    exchange.ab_test = {enabled: true, split: {v1: 50, v2: 50}, ...}
    |
    v
Step 3: Run "steady" simulation for 10 minutes
    Exchange routes 50% to dsp-stable, 50% to dsp-canary
    |
    v
Step 4: Open Operations UI -> A/B Test Results:
    ┌──────────────────────────────────────────────┐
    │  A/B Test: DSP Bid Algorithm v2              │
    │  Duration: 10 minutes | Requests: 30,000     │
    │                                              │
    │  Metric        v1 (stable)  v2 (canary)      │
    │  ─────────────────────────────────────────── │
    │  Win rate      45.2%        52.1%    ↑ +6.9  │
    │  eCPM          $2.50        $2.20    ↓ -$0.30│
    │  Latency p99   12ms         14ms     ↑ +2ms  │
    │  Error rate    0.00%        0.00%    = 0      │
    │  Budget spent  $125         $110     ↓ -$15   │
    │                                              │
    │  Recommendation: v2 wins more at lower cost  │
    │  Statistical significance: 95% (sufficient)  │
    │                                              │
    │  [Promote v2] [Revert to v1] [Extend test]   │
    └──────────────────────────────────────────────┘
```

#### What Can Be A/B Tested Locally

| What | How | What you'd measure |
|---|---|---|
| Bid shading algorithm | Two DSP versions with different shading logic | Win rate, clearing price, budget efficiency |
| Targeting rules engine | Two DSP versions with different targeting evaluation | Match rate, bid volume, relevance |
| Auction strategy | Two Exchange versions (e.g. different pod-filling logic) | Revenue per break, fill rate, latency |
| Creative rotation | Two Ad Server versions with different bandit algorithms | CTR, conversion rate |
| Fraud scoring | Two Tracker versions with different scoring models | False positive rate, fraud catch rate |
| Floor price model | Two SSP versions with different dynamic floor logic | Fill rate, eCPM, publisher revenue |

#### Automated A/B Testing in CI

The same A/B workflow runs in CI as an automated test:

```yaml
# .github/workflows/ab-test.yml
name: A/B Test - DSP Bid Algorithm
on:
  pull_request:
    paths: ['pkg/auction/shading.go', 'cmd/dsp/**']

jobs:
  ab-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Start k3s
        run: make setup-ci
      - name: Deploy stable (from main)
        run: make deploy-stable
      - name: Deploy canary (from PR branch)
        run: make deploy-canary service=dsp
      - name: Seed and simulate
        run: |
          make seed profile=standard
          make simulate profile=steady duration=5m
      - name: Collect metrics
        run: make ab-metrics service=dsp
      - name: Assert pass criteria
        run: |
          # Canary must not be worse than stable on key metrics
          make ab-assert \
            win_rate_delta_min=-2% \
            latency_p99_max=+20% \
            error_rate_max=0.1%
      - name: Report results
        run: make ab-report >> $GITHUB_STEP_SUMMARY
```

PR shows the A/B test results as a comment:

```
## A/B Test Results: DSP Bid Algorithm

| Metric | Stable | Canary | Delta | Pass? |
|---|---|---|---|---|
| Win rate | 45.2% | 52.1% | +6.9% | ✓ (min: -2%) |
| Latency p99 | 12ms | 14ms | +16% | ✓ (max: +20%) |
| Error rate | 0.00% | 0.00% | 0% | ✓ (max: 0.1%) |

**Result: PASS** - canary is not worse than stable on all criteria.
```

This means **no code change that affects bidding, targeting, or scoring can merge without an automated A/B test proving it's not a regression.** The same test runs locally (developer validates) and in CI (automated gate).

#### Implementation

| Component | Location |
|---|---|
| Canary K8s manifests | `k8s/base/{service}/deployment-canary.yaml` - template for canary deployment |
| Tilt canary buttons | Tiltfile - manual trigger resources for deploy/promote/rollback |
| A/B test config | Live config store + Operations UI |
| Metrics comparison | `cmd/reporting --mode=ab-compare` - collects v1 vs v2 metrics, outputs comparison |
| CI A/B workflow | `.github/workflows/ab-test.yml` |
| Pass criteria config | `profiles/ab-tests/{service}.yaml` - per-service pass/fail thresholds |

### Chaos Testing

Chaos testing proves the system handles failures gracefully in practice, not just in theory. Combined with A/B testing: "does the new code perform better AND survive failures?"

#### Testing Modes

| Mode | What it does | When to run |
|---|---|---|
| **Normal A/B** | Compare v1 vs v2 under normal conditions | Every PR with algorithm changes |
| **Chaos A/B** | Compare v1 vs v2 while injecting failures | Before promoting to prod, nightly |
| **Chaos regression** | Run stable code with chaos to verify resilience baseline | Nightly, after infra changes |

```
Normal A/B test:
    Simulation (steady, 5min) -> compare v1 vs v2 -> v2 wins on metrics

Chaos A/B test (same simulation + failures):
    Simulation (steady, 5min)
        + Kill Redis at t=60s, restore at t=90s
        + Kill one NATS node at t=120s
        + Add 200ms latency to DSP at t=180s
    -> compare v1 vs v2
    -> "v2 is faster, AND recovers from Redis failure in 8s (v1 took 15s)"
```

#### Chaos Scenarios

| Scenario | What it injects | What it tests | How |
|---|---|---|---|
| **Pod kill** | Randomly kill a service pod | Graceful shutdown, K8s restart, zero event loss | `kubectl delete pod {random}` |
| **Redis failure** | Kill Redis for 30s | Budget fallback to Postgres, frequency cap degradation, cache recovery via NATS replay | `kubectl delete pod redis-0` |
| **NATS partition** | Block NATS network for 30s | Event buffering in Tracker, consumer catch-up, no data slippage | Network policy injection |
| **Postgres standby failure** | Kill read replica | Read traffic failover to primary via PgBouncer | `kubectl delete pod postgres-standby-0` |
| **DSP latency** | Add 200ms latency to DSP responses | Exchange timeout handling, auction completes without slow DSP | `tc qdisc add` on DSP pod |
| **CPU throttle** | Limit a service to 10% CPU | Pacing under resource pressure, HPA response time | K8s resource limit patch |
| **Minio/S3 failure** | Make object storage unavailable | Creative fallback/default ads served, pipeline queues backpressure | `kubectl delete pod minio-0` |
| **Exchange channel failure** | Kill one exchange instance (e.g. video) | Other channels unaffected, traffic for that channel errors gracefully | `kubectl scale deployment exchange-video --replicas=0` |
| **Reporting crash mid-processing** | Kill reporting pod during event consumption | Idempotent replay on restart, no double-billing, no event loss | `kubectl delete pod reporting-0` |
| **Full infra restart** | Restart all infra (Postgres, NATS, Redis) simultaneously | Full system recovery, budget reconstruction, consumer catch-up | Script kills all infra pods |

#### Pass/Fail Criteria

| Metric | Normal test | Chaos test | Why different |
|---|---|---|---|
| Event loss | 0% | 0% | This is the whole point - zero data slippage even under failure |
| Error rate | < 0.1% | < 5% during failure, < 0.1% after recovery | Some errors OK during failure, must recover fully |
| Recovery time | N/A | < 30 seconds after failure ends | System must self-heal quickly |
| Budget accuracy | Exact match | Within 0.1% | Minor drift during Redis recovery, corrected by NATS replay |
| Impression tracking | 100% | > 99.5% during failure, 100% after NATS replay catches up | JetStream guarantees eventual delivery |
| Double-billing | 0% | 0% | Idempotent consumers must prevent this even during chaos |
| Latency (post-recovery) | Normal p99 | Within 20% of normal p99 within 60s of recovery | System shouldn't stay degraded |

#### Chaos Profiles

Like simulation profiles, chaos scenarios are configurable YAML files:

```yaml
# profiles/chaos/redis_failure.yaml
name: "Redis Failure and Recovery"
steps:
  - at: 30s
    action: kill_pod
    target: redis-0
    description: "Kill Redis primary"
  - at: 60s
    action: verify
    check: "dsp_budget_fallback_active == true"
    description: "Verify DSP fell back to Postgres for budgets"
  - at: 90s
    action: restore_pod
    target: redis-0
    description: "Redis comes back"
  - at: 120s
    action: verify
    check: "redis_budget_recovery_completed == true"
    description: "Verify budget recovery via NATS replay"
  - at: 150s
    action: verify
    check: "all_metrics_within_normal_range == true"
    description: "System fully recovered"

pass_criteria:
  event_loss: 0%
  budget_accuracy: 99.9%
  recovery_time_max: 30s
  double_billing: 0%
```

```yaml
# profiles/chaos/cascade_failure.yaml
name: "Cascading Infrastructure Failure"
steps:
  - at: 30s
    action: kill_pod
    target: redis-0
    description: "Redis fails"
  - at: 60s
    action: kill_pod
    target: postgres-standby-0
    description: "Postgres standby fails (while Redis is still down)"
  - at: 90s
    action: inject_latency
    target: nats
    latency_ms: 500
    description: "NATS gets slow"
  - at: 120s
    action: restore_all
    description: "Everything comes back"
  - at: 180s
    action: verify
    check: "full_system_healthy"
    description: "Complete recovery verified"

pass_criteria:
  event_loss: 0%
  recovery_time_max: 60s
```

#### Running Chaos Tests

**Locally (Tilt buttons):**

```
Tilt Dashboard:
    [Kill Redis]  [Kill NATS Node]  [Add DSP Latency]  [Restore All]

    Chaos status: Redis killed at 14:32:05, recovered at 14:32:35 (30s)
    Budget recovery: completed via NATS replay (1,247 events replayed)
    Event loss: 0%
```

**In CI (nightly):**

```yaml
# .github/workflows/chaos-test.yml
name: Chaos Regression Test
on:
  schedule:
    - cron: '0 3 * * *'  # nightly at 3am

jobs:
  chaos:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Start k3s + deploy
        run: make setup-ci && make deploy
      - name: Seed data
        run: make seed profile=standard
      - name: Run chaos + simulation
        run: |
          # Start simulation in background
          make simulate profile=steady duration=5m &
          # Run chaos scenario
          make chaos profile=redis_failure
          make chaos profile=cascade_failure
          # Wait for simulation to finish
          wait
      - name: Verify chaos pass criteria
        run: make chaos-verify
      - name: Report results
        run: make chaos-report >> $GITHUB_STEP_SUMMARY
```

**Combined with A/B testing:**

```
make ab-test service=dsp chaos=redis_failure

This runs:
    1. Deploy stable + canary
    2. Start simulation
    3. At t=60s: kill Redis (chaos)
    4. At t=90s: restore Redis
    5. Compare v1 vs v2 metrics during AND after chaos
    6. Report: "v2 recovered in 8s (v1 took 15s). v2 lost 0 events (v1 lost 3)."
```

#### What Chaos Validates Per Feature

| Feature | What chaos proves |
|---|---|
| **Redis budget recovery** | NATS replay reconstructs exact budget after Redis failure |
| **Idempotent consumers** | No double-billing after NATS redelivery during chaos |
| **Circuit breakers** | DSP stops calling Redis when it's down, falls back to Postgres |
| **Graceful shutdown** | Killed pods drain in-flight requests, flush buffers |
| **NATS JetStream persistence** | Events survive consumer crashes, replayed on restart |
| **PgBouncer failover** | Reads shift to primary when standby is killed |
| **HPA scaling** | New pods spin up when existing pods are killed |
| **Cache invalidation** | L1 caches rebuild correctly after Redis recovery |
| **DuckDB single-writer** | Reporting pod restart doesn't corrupt DuckDB file |
| **Pacing under pressure** | Budget pacing stays correct even during CPU throttle |

#### Implementation

| Component | Location |
|---|---|
| Chaos runner | `pkg/chaos/` - pod kill, network injection, latency injection, restore |
| Chaos profiles | `profiles/chaos/*.yaml` - scenario definitions |
| Chaos CLI | `cmd/simulator --chaos=redis_failure` - run chaos alongside simulation |
| Chaos Tilt buttons | Tiltfile - manual chaos injection buttons |
| Chaos verification | `pkg/chaos/verify.go` - check pass criteria after chaos run |
| CI chaos workflow | `.github/workflows/chaos-test.yml` |
| Chaos + A/B combo | `make ab-test service=dsp chaos=redis_failure` |

---

## Authentication, Roles, and Permissions

### Account Types

The platform has five account types. Each sees a different dashboard and has access to different features.

| Account type | Who they are | What they see | Multi-tenant? |
|---|---|---|---|
| **Advertiser** | Buys ad inventory | Campaigns, creatives, audiences, billing, reports | Yes - isolated to their data |
| **Publisher** | Sells ad inventory | Placements, deals, quality controls, earnings, pipeline, reports | Yes - isolated to their data |
| **Agency** | Manages multiple advertiser accounts | All advertiser features across multiple accounts, cross-account reporting | Yes - sees only their managed accounts |
| **Platform Staff** | Operations team | Moderation, fraud review, publisher/advertiser support, reconciliation | No - sees all accounts (read + limited write) |
| **Platform Admin** | Super administrators | Everything + user management, SSO config, live config, infrastructure ops | No - full access to everything |

### Role Hierarchy Per Account Type

Each account type has its own set of roles:

**Advertiser Roles:**

| Role | Campaigns | Creatives | Audiences | Billing | Reports | Team | Settings |
|---|---|---|---|---|---|---|---|
| Owner | Full | Full | Full | Full | Full | Full | Full |
| Manager | Create, edit, pause | Upload, manage | Create, upload | View | Full | Invite (below own role) | View |
| Analyst | View | View | View | View | Full + export | No | No |
| Finance | No | No | No | Full (invoices, topup, disputes) | Billing reports only | No | No |
| Viewer | View | View | View | View balance | View (no export) | No | No |

**Publisher Roles:**

| Role | Placements | Deals | Quality Controls | Earnings | Reports | Pipeline | Team | Settings |
|---|---|---|---|---|---|---|---|---|
| Owner | Full | Full | Full | Full | Full | Full | Full | Full |
| Manager | Create, edit | Create, edit | Manage | View | Full | Upload files | Invite | View |
| Ad Ops | Edit floor prices, quality controls | View | Full | No | Fill rate, yield | No | No | No |
| Analyst | View | View | View | View | Full + export | View status | No | No |
| Finance | No | No | No | Full (payouts, disputes) | Revenue reports only | No | No | No |
| Viewer | View | View | View | View earnings | View (no export) | No | No | No |

**Agency Roles:**

| Role | Managed accounts | Cross-account reports | Billing | Team |
|---|---|---|---|---|
| Agency Admin | Full access to all managed advertiser accounts | Yes | View all | Full |
| Account Manager | Full access to assigned accounts only | Own accounts only | View assigned | No |
| Agency Analyst | View-only across all managed accounts | Yes | No | No |

**Platform Staff Roles:**

| Role | Moderation | Fraud | Support | Config | Operations | User management |
|---|---|---|---|---|---|---|
| Admin | Full | Full | Full | Full | Full | Full |
| Operations | View | Full | Full | View | Full | No |
| Ad Ops | Full (creative review, publisher approval) | View | View | No | No | No |
| Support | View | View | Full (disputes, tickets) | No | No | No |
| Finance | No | No | Billing disputes | No | No | No |

### Permission Model

Permissions are structured as `resource:action`:

```
campaigns:create
campaigns:read
campaigns:update
campaigns:delete
campaigns:submit
campaigns:approve          # platform staff only
campaigns:reject           # platform staff only
campaigns:pause
campaigns:resume
creatives:upload
creatives:approve          # platform staff only
billing:view
billing:topup
billing:dispute
config:read
config:update              # platform admin only
ops:ab_test:create         # platform staff only
ops:canary:promote         # platform staff only
moderation:queue:view      # platform staff only
moderation:approve         # platform staff only
```

Each role maps to a set of permissions. The Gateway checks permissions on every request:

```
Request arrives at Gateway
    |
    v
1. Extract JWT -> account_id, user_id, account_type, role
    |
    v
2. Look up permissions for this role:
    role "advertiser:manager" -> [campaigns:create, campaigns:read, campaigns:update, ...]
    |
    v
3. Check: does this request's required permission exist in the user's permissions?
    POST /v1/api/campaigns -> requires "campaigns:create"
    User has "campaigns:create"? -> Yes -> proceed
    |
    v
4. Multi-tenancy: inject account_id into gRPC metadata
    (even if permission check passes, they can only see their own data)
```

### JWT Token Structure

```json
{
    "sub": "user_789",
    "account_id": "adv_123",
    "account_type": "advertiser",
    "role": "manager",
    "permissions": ["campaigns:create", "campaigns:read", "campaigns:update", "campaigns:pause", "creatives:upload", "creatives:read", "billing:view", "reports:read"],
    "managed_accounts": null,
    "iat": 1716825600,
    "exp": 1716829200
}
```

For agency accounts, `managed_accounts` lists which advertiser accounts they can access:

```json
{
    "sub": "user_agency_001",
    "account_id": "agency_456",
    "account_type": "agency",
    "role": "account_manager",
    "permissions": ["campaigns:create", "campaigns:read", ...],
    "managed_accounts": ["adv_123", "adv_456", "adv_789"],
    "iat": 1716825600,
    "exp": 1716829200
}
```

### Agency Account Model

Agencies manage multiple advertiser accounts. They need cross-account access without seeing other agencies' clients.

```
Agency "MediaBuyers Inc" (agency_456)
    |
    +-- Manages: Advertiser "Acme Corp" (adv_123)
    +-- Manages: Advertiser "TechCo" (adv_456)
    +-- Manages: Advertiser "FoodBrand" (adv_789)
    |
    Agency user "Alice" (account_manager):
        Assigned to: adv_123, adv_456
        Can: create campaigns, manage budgets for these two accounts
        Cannot: access adv_789 (not assigned)
    |
    Agency user "Bob" (agency_admin):
        Assigned to: all managed accounts
        Can: everything across all three accounts + cross-account reporting
```

**Cross-account switching:**

```
Agency user is logged in
    |
    v
GET /v1/api/agency/accounts -> returns [adv_123, adv_456, adv_789]
    |
    v
User selects "Acme Corp" context
    POST /v1/api/agency/switch-context {account_id: "adv_123"}
    |
    v
JWT is reissued with account_id: "adv_123" (operating as Acme Corp)
    All subsequent API calls are in Acme Corp's context
    Multi-tenancy enforced as if they were logged into Acme Corp directly
    |
    v
User switches to cross-account view
    POST /v1/api/agency/switch-context {account_id: "all"}
    |
    v
Cross-account reporting available
    GET /v1/api/reports/campaigns -> returns campaigns across all managed accounts
```

**Agency API endpoints:**
- `GET  /v1/api/agency/accounts` - list managed accounts
- `POST /v1/api/agency/accounts/{id}/link` - link an advertiser account to agency
- `DELETE /v1/api/agency/accounts/{id}/unlink` - unlink an advertiser account
- `POST /v1/api/agency/switch-context` - switch operating context
- `GET  /v1/api/agency/reports/cross-account` - cross-account reporting

### Role-Gated Dashboard

The HTMX dashboard shows different navigation and content based on the user's account type and role:

**Advertiser (Manager) sees:**
```
Dashboard | Campaigns | Creatives | Audiences | Reports | Billing
```

**Publisher (Owner) sees:**
```
Dashboard | Placements | Deals | Quality Controls | Pipeline | Reports | Earnings | Settings
```

**Agency (Admin) sees:**
```
Dashboard | Accounts | Campaigns (cross-account) | Reports (cross-account) | Billing
[Switch Account: Acme Corp ▼]
```

**Platform Staff (Operations) sees:**
```
Dashboard | Moderation | Fraud | Support | Operations | Config | Reports (all accounts)
```

**Platform Admin sees:**
```
Everything above + Users | SSO | Infrastructure | Audit Log
```

The Gateway renders different navigation templates based on `account_type` and `role` from the JWT. No client-side role checking - the server only renders what the user is allowed to see.

### Permission Enforcement Points

Permissions are checked at three levels for defence in depth:

| Level | How | Catches |
|---|---|---|
| **Gateway middleware** | Check permission before proxying to gRPC service | Unauthorized API calls |
| **gRPC service** | Service validates role/permission from gRPC metadata | Defence against misconfigured Gateway |
| **Database (RLS)** | Postgres row-level security filters by account_id | Defence against code bugs skipping permission checks |

All three must pass. A request that gets past the Gateway but has wrong permissions is still blocked by the service. A service bug that skips permission checks is still blocked by RLS.

### API Key Permissions

API keys inherit the creating user's permissions but can be scoped down:

```json
{
    "api_key": "ak_xxxxxxxx",
    "account_id": "adv_123",
    "created_by": "user_789",
    "scoped_permissions": ["campaigns:read", "reports:read"],
    "tier": "standard",
    "rate_limit": 500
}
```

The API key can only have permissions that the creating user has. A viewer can't create an API key with `campaigns:create` permission.

### Implementation

| Component | Location |
|---|---|
| Permission definitions | `pkg/auth/permissions.go` - all `resource:action` constants |
| Role-to-permission mapping | `pkg/auth/roles.go` - role -> []permission mapping per account type |
| Permission middleware | `pkg/middleware/auth.go` - JWT extraction, permission check, account_type enforcement |
| Agency context switching | `pkg/auth/agency.go` - context switch, managed account validation |
| Role-gated templates | `web/templates/` - conditional navigation rendering based on role |
| API key scoping | `pkg/auth/apikeys.go` - permission inheritance and scoping |

---

## Security

### Secret Management

**SOPS (Mozilla)** for now - encrypted secret files committed to the repo, decrypted at deploy time. No extra infrastructure. Upgrade to Vault later if needed.

| Environment | Approach |
|---|---|
| Local | Plaintext K8s Secrets (it's your laptop) |
| Staging | SOPS-encrypted files in `k8s/overlays/staging/secrets.enc.yaml` |
| Prod | SOPS-encrypted files in `k8s/overlays/prod/secrets.enc.yaml` |

SOPS decryption key lives in GitHub Actions secrets (never in the repo). CI/CD decrypts at deploy time.

### Secrets Inventory

| Secret | Used by | Stored in |
|---|---|---|
| Postgres credentials | All services with DB access | K8s Secret (SOPS-encrypted for staging/prod) |
| Redis password | DSP, Ad Server, Gateway, Reporting | K8s Secret |
| NATS credentials | Tracker, Exchange, DSP, Reporting | K8s Secret |
| S3 access key + secret | Ad Server, Pipeline | K8s Secret |
| JWT signing key | Gateway | K8s Secret |
| Tracker HMAC signing key | Tracker, Ad Server (generates URLs) | K8s Secret |
| SOPS decryption key | CI/CD pipeline only | GitHub Actions secret |

### Inter-Service Communication Security

| Layer | Approach |
|---|---|
| mTLS | Not for MVP. K8s network is trusted by default. Add Linkerd (lighter than Istio) later if threat model requires it. |
| gRPC auth | Internal services pass a shared cluster token in gRPC metadata. Prevents accidental external access. |
| OpenRTB endpoints | API key per DSP/SSP, validated on every request. |
| Tracker endpoints | Signed URLs (`sig` param) using HMAC-SHA256. Tamper protection without auth. |

### K8s Network Policies

Restrict which services can talk to which. Prevents lateral movement even without mTLS.

| Service | Allowed inbound from | Allowed outbound to |
|---|---|---|
| Gateway | Internet (ingress) | DSP, SSP, Exchange, Reporting, Ad Server (gRPC) |
| Exchange | SSP, Gateway (gRPC) | DSP (OpenRTB), Ad Server (gRPC), NATS |
| DSP | Exchange (OpenRTB), Gateway (gRPC) | Postgres, Redis, NATS |
| SSP | Gateway (gRPC) | Exchange (gRPC), Postgres |
| Ad Server | Exchange (gRPC) | Tracker (gRPC), Object storage, Redis |
| Tracker | Internet (pixel endpoints), Ad Server (gRPC) | NATS, Redis |
| Reporting | Gateway (gRPC) | NATS, Postgres, DuckDB/ClickHouse |
| Pipeline | Gateway (gRPC) | Object storage, Postgres, DuckDB/ClickHouse |
| Postgres | DSP, SSP, Reporting, Pipeline, Gateway | None |
| Redis | DSP, Ad Server, Gateway, Reporting | None |
| NATS | Tracker, Exchange, DSP, Reporting | None |

Defined as K8s NetworkPolicy manifests in `k8s/base/`. Enforced in all environments.

### Application Security

| Concern | Approach |
|---|---|
| Input validation | All external inputs validated at the boundary (gateway, tracker, OpenRTB). Protobuf validation for gRPC. |
| SQL injection | Parameterised queries only. No string concatenation. Enforced by `pkg/store/` abstractions. |
| XSS | Go's `html/template` auto-escapes. No raw HTML injection. |
| CORS | Strict origin allowlist on gateway. Configured per environment via Kustomize. |
| API key hashing | API keys stored as bcrypt hashes in Postgres, never plaintext. |
| Tracker signatures | HMAC-SHA256 for URL signatures. Keys rotated via config, old keys valid for a grace period. |
| Rate limiting | Already covered - prevents brute force on auth, abuse of tracker/API. |

---

## Billing and Financial Reconciliation

### Single Source of Truth: The AuctionWinEvent

The core insight: don't calculate spend independently in three places. Make every system derive cost from **one canonical event**.

```
Auction completes, winner selected
    |
    v
Exchange publishes AuctionWinEvent to NATS
    {trace_id, campaign_id, placement_id, clearing_price, bid_model, timestamp}
    |
    +----------+-----------+
    |          |           |
    v          v           v
   DSP     Tracker    Reporting (unified with Billing)
   DECRBY   joins      writes to analytics store AND
   budget   cost to    accrues spend in billing tables
   by       impression in the same transaction
   clearing via
   price    trace_id
```

Three consumers (not four). Reporting and Billing are the same service, processing each event once and writing to both stores atomically. No reconciliation needed - one consumer, one event, both records written together.

The tracker still records impressions/clicks/conversions, but it doesn't independently calculate cost. Cost comes from the `AuctionWinEvent`, matched by trace_id.

### Billing Flow

```
Exchange publishes AuctionWinEvent (single source of truth)
    |
    v
Reporting service (unified with billing) consumes event:
    1. Write to analytics store (DuckDB/ClickHouse) -> reporting data
    2. Accrue spend in Postgres billing tables -> advertiser_spend, publisher_revenue
    3. Both writes in the same processing step
    |
    v
Fraud scoring (batch) marks events as invalid
    |
    v
Billable spend = accrued spend - fraud-excluded spend (tracked as adjustments)
    |
    v
Scheduled job (cmd/reporting --mode=scheduler):
    Daily snapshot locks the numbers
    Invoice / payout generation on schedule
```

### Reconciliation (Simplified by Unified Service)

Because reporting and billing are the same service processing each event once, the old three-way reconciliation is no longer needed. Instead, a simpler verification runs daily:

```
For each campaign, for each day:
    1. Count AuctionWinEvents published by Exchange
    2. Count budget decrements recorded by DSP
    3. Count rows written by Reporting service (analytics + billing are same count by construction)

    All counts match? -> Verified
    Count mismatch?   -> DSP missed an event (pipeline bug)
                         Flag exactly which trace_ids are missing
```

| Check | What it verifies | If it fails |
|---|---|---|
| Exchange published == DSP consumed | DSP received every win event | Missing events in DSP - budget not decremented |
| Exchange published == Reporting consumed | Every win is in both analytics store AND billing tables | Missing data in reports or billing |
| DSP total == Reporting total | Budget decrements match billing accruals | Arithmetic bug |
| Analytics rows == Billing rows | Same service wrote both, should always match | If mismatch = bug in unified consumer |

Since reporting and billing are the same service, the last check should always pass. If it doesn't, there's a bug in the event processing code itself (not a pipeline issue). This is the "zero data slippage" proof applied to money.

### Billing Models

| Model | How it works | Billed on |
|---|---|---|
| CPM (cost per mille) | Pay per 1000 impressions | Impression event |
| CPC (cost per click) | Pay per click | Click event |
| CPA (cost per action) | Pay per conversion | Conversion event |
| Flat fee | Fixed daily/monthly spend | Schedule |

The campaign's bid strategy determines the model. Billing system handles all of them.

### How Billing Models Interact with AuctionWinEvent

The `AuctionWinEvent` is always the source of truth for *what happened in the auction*. But the actual billing depends on the campaign's model:

**CPM (simple path):**
```
AuctionWin (clearing_price = $2 CPM)
    -> Bill immediately: advertiser charged, publisher paid
    -> Budget decremented by clearing_price
```

**CPC (reserve and settle):**
```
AuctionWin (clearing_price = $0.50 max CPC)
    -> Reserve: hold $0.50 from budget (not billed yet)
    -> Ad served, impression tracked
    -> Click arrives (matched via trace_id)?
        Yes -> Settle: bill actual CPC from campaign config, release remainder
        No  -> Release: return reserved amount to budget
    -> Reservation expires after configurable window (e.g. 24h)
```

**CPA (reserve and settle):**
```
AuctionWin (clearing_price = $5.00 max CPA)
    -> Reserve: hold $5.00 from budget
    -> Ad served, impression tracked, maybe click tracked
    -> Conversion arrives (matched via trace_id, within attribution window)?
        Yes -> Settle: bill actual CPA from campaign config, release remainder
        No  -> Release: return reserved amount after attribution window closes
    -> Attribution window: configurable per campaign (e.g. 7 days)
```

Budget handling in Redis:
- **CPM:** `DECRBY` on win (immediate)
- **CPC/CPA:** `DECRBY` on win (reservation), `INCRBY` if no click/conversion arrives (release)

### Reconciliation per Billing Model

| Model | Reconciliation check |
|---|---|
| CPM | AuctionWinEvents == billing accruals (count and total) |
| CPC | ClickEvents with matching auction trace_ids == billing accruals |
| CPA | ConversionEvents with matching auction trace_ids == billing accruals |
| All | Outstanding reservations older than window == released (no stuck holds) |

### Billing Entities

| Entity | What it tracks |
|---|---|
| Advertiser balance | Pre-paid credit or credit limit. Spend deducted as events are billed. |
| Publisher earnings | Revenue accrued from impressions on their placements. Platform takes a margin. |
| Invoice | Periodic statement of advertiser spend (daily/weekly/monthly, configurable). |
| Payout | Periodic statement of publisher earnings minus platform fee. |
| Adjustment | Manual credit/debit for disputes, fraud refunds, billing errors. |
| Reconciliation record | Daily comparison of all three spend sources. Flags discrepancies. |

### Invoice and Payout Generation

| Step | When | What |
|---|---|---|
| Daily spend snapshot | End of day CronJob | Lock the day's spend after reconciliation completes |
| Invoice generation | Configurable per advertiser (weekly/monthly) | Aggregate daily snapshots into invoice |
| Publisher payout calculation | Same schedule | Publisher revenue = clearing prices on their placements - platform fee % |
| Adjustment application | Any time | Credits/debits applied to next invoice/payout |

### Custom Report Builder

Publishers and advertisers need to build their own reports tailored to their business. A drag-and-drop report builder in the dashboard.

**How it works:**

| Step | What the user does |
|---|---|
| 1. Pick metrics | Select from: impressions, clicks, conversions, spend, revenue, CTR, eCPM, ROAS, fill rate, etc. |
| 2. Pick dimensions | Group by: campaign, creative, placement, publisher, geo, device, day, hour, etc. |
| 3. Pick filters | Filter by: date range, specific campaigns, placements, geo, device, min/max values |
| 4. Pick format | View as: table, chart (line, bar, pie), or export (CSV, PDF) |
| 5. Save & schedule | Save report template, optionally schedule delivery via email or webhook |

**Report templates** are saved in Postgres per account. A user can have many saved reports. Scheduled reports run as CronJobs and deliver results to configured destinations.

**Pre-built report templates** shipped with the platform:

| Template | Audience | Contains |
|---|---|---|
| Campaign Performance | Advertiser | Impressions, clicks, CTR, conversions, spend, ROAS by campaign/day |
| Creative Performance | Advertiser | CTR, conversion rate by creative, auto-highlights best/worst performers |
| Publisher Revenue | Publisher | Impressions, revenue, fill rate, eCPM by placement/day |
| Traffic Quality | Publisher | Fraud rate, IVT percentage, flagged events by placement |
| Billing Summary | Advertiser | Spend by campaign, adjustments, balance remaining, invoice history |
| Payout Summary | Publisher | Earnings by placement, platform fees, payout history |
| Reconciliation | Platform (internal) | Three-way spend comparison, discrepancies, flagged campaigns |
| Platform P&L | Platform (internal) | Total advertiser spend, total publisher payouts, margin, trends |

**Billing-specific reports:**

- **Advertiser invoice report** - attached to every invoice. Breaks down spend by campaign, creative, placement, day. Shows fraud-excluded events separately. Advertiser can verify every line.
- **Publisher payout report** - attached to every payout. Breaks down revenue by placement, day, ad type. Shows platform fee calculation.
- **Reconciliation report** - internal. Shows the three-way comparison, highlights any discrepancy > threshold, links to trace IDs for investigation.

### Dashboard Views

| View | Audience | What it shows |
|---|---|---|
| Billing overview | Advertiser | Current balance, spend by day/campaign, invoices, payment history, adjustments |
| Earnings overview | Publisher | Revenue by day/placement, payouts, pending earnings |
| Custom reports | Both | Report builder, saved reports, scheduled reports |
| Platform finance | Internal | Total revenue, total payouts, margin, reconciliation status |
| Reconciliation | Internal | Discrepancy report, flagged campaigns, drill-down to trace level |

### Implementation

| Component | Location |
|---|---|
| Billing service | `cmd/billing/` - invoice generation, payout calculation |
| Billing logic | `pkg/billing/` - spend calculation, invoice models, reconciliation engine |
| Report builder logic | `pkg/reporting/builder.go` - query construction from user-selected dimensions/metrics/filters |
| Report templates | `pkg/reporting/templates.go` - pre-built report definitions |
| Billing tables | Postgres - invoices, payouts, adjustments, balances, saved reports |
| Reconciliation | Daily verification via `cmd/reporting --mode=scheduler` (same service, simpler check) |
| Report + billing scheduler | K8s CronJob using `cmd/reporting --mode=scheduler` - reports, invoices, payouts, daily snapshots |
| Dashboard | Gateway - billing views, earnings views, report builder UI |

### Gateway API Endpoints

**Billing (advertiser):**
- `GET    /v1/api/billing/balance` - current balance
- `GET    /v1/api/billing/invoices` - invoice list
- `GET    /v1/api/billing/invoices/{id}` - invoice detail with line-item breakdown
- `GET    /v1/api/billing/invoices/{id}/report` - downloadable invoice report (CSV/PDF)

**Earnings (publisher):**
- `GET    /v1/api/earnings/summary` - earnings overview
- `GET    /v1/api/earnings/payouts` - payout list
- `GET    /v1/api/earnings/payouts/{id}/report` - downloadable payout report

**Reports (both):**
- `GET    /v1/api/reports/builder` - available metrics, dimensions, filters
- `POST   /v1/api/reports/query` - run a custom report
- `POST   /v1/api/reports/saved` - save a report template
- `GET    /v1/api/reports/saved` - list saved reports
- `PUT    /v1/api/reports/saved/{id}` - update saved report (including schedule)
- `DELETE /v1/api/reports/saved/{id}` - delete saved report

**Reconciliation (internal):**
- `GET    /v1/api/reconciliation/daily?date=` - daily reconciliation results
- `GET    /v1/api/reconciliation/flagged` - campaigns with discrepancies

---

## Configuration Management

### Config Layers

| Layer | Where | Changed by | Examples |
|---|---|---|---|
| **Service defaults** | Code (`pkg/config/defaults.go`) | Code change + deploy | Sensible defaults if nothing is set in DB |
| **Infrastructure** | Kustomize overlays (env vars) | Deploy | DB connection strings, Redis URLs, S3 buckets, replica counts |
| **Live / runtime** | Postgres config table + dashboard UI | Dashboard (no deploy) | Rate limits, fraud thresholds, bid timeout, feature flags, pacing multipliers |

**Rule: if you might want to change it without a deploy, it goes in live config.**

### Live Config Store

Postgres table that services poll for runtime configuration:

```sql
CREATE TABLE config (
    key         TEXT PRIMARY KEY,
    value       JSONB NOT NULL,
    service     TEXT NOT NULL,
    description TEXT,
    updated_at  TIMESTAMP,
    updated_by  TEXT
);
```

### How Services Load Config

```
Service starts
    |
    v
Load defaults from code (pkg/config/defaults.go)
    |
    v
Override with env vars (Kustomize - infrastructure config)
    |
    v
Override with Postgres config table (live config)
    |
    v
Subscribe to NATS: adtech.cache.invalidate.config
    |
    v
On change notification: refresh from Postgres immediately
On interval (fallback): poll Postgres every 30s
```

NATS makes changes near-instant. Polling is the fallback if NATS misses a message. Either way, config updates take effect within seconds without a deploy.

### Config Change Flow

```
Admin changes config in dashboard
    |
    v
Gateway writes to Postgres config table (+ audit log in same transaction)
    |
    v
Publishes NATS: adtech.cache.invalidate.config {key: "exchange.bid_timeout_ms"}
    |
    v
All Exchange instances receive, reload config from Postgres
New bid timeout takes effect immediately
```

### Example Live Configs

| Key | Service | Default | Description |
|---|---|---|---|
| `exchange.bid_timeout_ms` | Exchange | 100 | Max time to wait for DSP bid response |
| `exchange.max_dsp_fanout` | Exchange | 10 | Max DSPs to call per auction |
| `tracker.rate_limit_per_ip` | Tracker | 100 | Max events per second per IP |
| `fraud.score_threshold` | Tracker | 0.7 | Events above this score excluded from billing |
| `dsp.pacing_multiplier` | DSP | 1.0 | Global pacing speed adjustment |
| `adserver.frequency_cap_default` | Ad Server | 3 | Default frequency cap per user per day |
| `pipeline.max_concurrent_files` | Pipeline | 5 | Max files processed in parallel |
| `reporting.cache_ttl_seconds` | Reporting | 60 | Dashboard query cache TTL |

### Dashboard UI

- Config keys grouped by service in a table
- Current value, default value, last changed by/when
- Edit inline - saves immediately, takes effect within seconds
- History per key (links to audit log)
- "Reset to default" button per key
- Diff view: current vs default for all keys

### Implementation

| Component | Location |
|---|---|
| Config loader | `pkg/config/` - loads defaults, env vars, then live config from Postgres |
| Config defaults | `pkg/config/defaults.go` - all default values defined in code |
| Config watcher | `pkg/config/watcher.go` - NATS subscription + polling fallback |
| Dashboard API | Gateway: `GET /v1/api/config`, `PUT /v1/api/config/{key}` |

---

## Audit Logging

### What Gets Audited

| Category | Events | Why |
|---|---|---|
| Campaign changes | Created, updated (budget, targeting, schedule, bid strategy), paused, deleted | Money is involved - every change affects spend |
| Creative changes | Uploaded, updated, approved, rejected | What was served to users |
| Account changes | Created, role changed, permissions modified, API keys generated/revoked | Access control trail |
| Publisher changes | Placement added/removed, floor price changed, blocklist updated | Affects revenue and ad quality |
| Financial events | Budget top-ups, invoice generated, payment processed, credit issued | Money trail |
| Fraud actions | Publisher flagged, events excluded from billing, blocklist updated | Dispute resolution |
| Config changes | Rate limits adjusted, fraud thresholds changed | Operational trail |
| Data access | Report exported, PII accessed, audience segment downloaded | Compliance |

### Audit Record Structure

```go
type AuditEvent struct {
    ID           string    // unique event ID
    Timestamp    time.Time // when
    ActorID      string    // who (user ID or "system" for automated)
    ActorIP      string    // from where
    Action       string    // what (e.g. "campaign.update")
    ResourceType string    // what type (e.g. "campaign")
    ResourceID   string    // which one
    Changes      []Change  // what changed (field, old value, new value)
    Reason       string    // optional - why
}
```

Example: user increases a campaign budget and expands geo targeting:

```json
{
    "timestamp": "2026-05-27T14:32:00Z",
    "actor_id": "user_456",
    "actor_ip": "192.168.1.50",
    "action": "campaign.update",
    "resource_type": "campaign",
    "resource_id": "camp_123",
    "changes": [
        {"field": "daily_budget", "old": "100.00", "new": "500.00"},
        {"field": "targeting.geo", "old": "[\"UK\"]", "new": "[\"UK\",\"DE\"]"}
    ]
}
```

### Storage

| Store | What | Retention |
|---|---|---|
| Postgres `audit_log` table | Recent audit events, queryable by resource | 90 days |
| Analytics store (DuckDB/ClickHouse) | Long-term audit archive, rolled up from Postgres | Forever |

Same rollup pattern as event data. A CronJob moves entries older than 90 days from Postgres to the analytics store.

The Postgres table is **append-only** - no UPDATE or DELETE grants, even for admins. Audit logs are immutable.

### How It Works

```
User changes campaign budget in dashboard
    |
    v
Gateway extracts actor ID + IP from JWT/request
    |
    v
Gateway calls DSP via gRPC (actor context in metadata)
    |
    v
DSP updates campaign in Postgres + writes audit record in same transaction
    |
    v
Audit record is queryable immediately
```

The audit log write and the actual change happen in the **same database transaction**. If the change fails, no audit record. If the audit write fails, the change rolls back. They're always consistent.

### Dashboard

- **Per-resource audit trail** - "show me everything that happened to campaign X" - ordered list of changes with who/when/what
- **Per-user activity log** - "show me everything user Y did today"
- **Search** - find all changes to budgets above $1000, all API key revocations, all fraud-related actions
- **Export** - download audit trail as CSV for compliance/legal

### Implementation

| Component | Location |
|---|---|
| Audit library | `pkg/audit/` - `Log()` function, extracts actor context from gRPC metadata |
| Audit middleware | `pkg/middleware/audit.go` - injects actor context into gRPC calls |
| Audit rollup | CronJob in `cmd/rollup/` - moves old Postgres entries to analytics store |
| Dashboard views | Gateway serves audit trail UI per resource |

---

## Database Migrations

### Tool: goose

Go-native migration tool. Migrations are plain SQL files with `-- +goose Up` and `-- +goose Down` sections. Embedded into the Go binary via `embed.FS` - no separate files to deploy in containers.

### Migration Files

```
migrations/
  # Core entities
  001_create_accounts.sql
  002_create_team_members.sql
  003_create_api_keys.sql
  004_create_campaigns.sql
  005_create_creatives.sql
  006_create_publishers.sql
  007_create_placements.sql
  008_create_targeting_rules.sql
  009_create_audience_segments.sql
  010_create_deals.sql

  # Identity and privacy
  011_create_identity_graph.sql
  012_create_advertiser_audiences.sql

  # Billing and finance
  013_create_advertiser_balances.sql
  014_create_invoices.sql
  015_create_payouts.sql
  016_create_adjustments.sql
  017_create_reconciliation_records.sql
  018_create_budget_reservations.sql

  # Reporting and config
  019_create_saved_reports.sql
  020_create_config.sql
  021_create_audit_log.sql

  # Webhooks and notifications
  022_create_webhooks.sql
  023_create_webhook_deliveries.sql
  024_create_notification_preferences.sql

  # Fraud
  025_create_fraud_blocklists.sql

  # Quality controls
  026_create_publisher_quality_controls.sql
  027_create_creative_review_queue.sql

  # ads.txt
  028_create_ads_txt_cache.sql

  # Row-level security policies
  029_enable_rls_policies.sql
```

Each file contains the exact SQL to run:

```sql
-- migrations/001_create_accounts.sql

-- +goose Up
CREATE TABLE accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('advertiser', 'publisher')),
    created_at TIMESTAMP DEFAULT now()
);

-- +goose Down
DROP TABLE accounts;
```

### How Migrations Run in K8s

```
Deploy starts
    |
    v
K8s Job: "migrate" (runs goose up)
    |
    +-- Succeeds --> K8s rolls out new service pods
    |
    +-- Fails ----> Deploy halts, alert fires, no new pods deployed
```

The migration runs as a **K8s Job** that must complete successfully before new service pods are deployed. Failed migration = no new code running against a broken schema.

### Production Rules

1. **Additive only** - add columns, add tables. Never rename or drop columns in the same release as code changes. Do it in two releases: first stop using the column, then drop it in the next release.
2. **Backwards compatible** - old code must still work with the new schema in case of app rollback.
3. **One change per migration** - small files, easy to debug, easy to rollback.
4. **Always test in staging first** - same migration runs in staging before prod. Staging breaks = prod is safe.
5. **Transactional** - each migration runs in a transaction (goose default). Fails = rolls back cleanly.
6. **Advisory locking** - only one migration runner at a time, prevents conflicts when multiple instances deploy.

### Commands

| Command | What it does |
|---|---|
| `goose up` | Runs all pending migrations in order |
| `goose down` | Rolls back the last migration |
| `goose status` | Shows which migrations have been applied |
| `goose reset` | Rolls back everything |

### Implementation

| Component | Location |
|---|---|
| Migration files | `migrations/*.sql` |
| Migration runner | `cmd/migrate/` - Go binary that embeds migration files and runs goose |
| K8s Job manifest | `k8s/base/migrate/` - runs before service deployments |

### Environment Workflow

| Environment | When migrations run |
|---|---|
| Local | Tilt runs the migrate job automatically on `tilt up` and when migration files change |
| Staging | CI/CD runs migrate job after merge to main, before deploying services |
| Prod | CI/CD runs migrate job during manual prod promotion, before deploying services |

---

## Backups and Disaster Recovery

### Database Backups (PostgreSQL)

| Environment | Strategy | Schedule | Retention |
|---|---|---|---|
| Local | Not needed (seed data is recreatable) | - | - |
| Staging | `pg_dump` via K8s CronJob | Daily | 7 days |
| Prod | WAL archiving + daily base backups to S3 | Continuous WAL, daily base | WAL: 7 days, base: 30 days |

**Point-in-time recovery (PITR):** In prod, WAL archiving allows restoring to any second within the retention window. If a bad migration corrupts data at 14:32, restore to 14:31.

**Pre-migration backup:** The CI/CD pipeline takes a `pg_dump` snapshot before every migration job runs. If the migration succeeds but the new code has a bug, you have an exact pre-migration snapshot to restore from.

### Analytics Store Backups

| Store | Strategy |
|---|---|
| DuckDB (local/staging) | File-level backup - it's a single file on a PersistentVolumeClaim. Copy to S3 daily. |
| ClickHouse (prod) | Built-in backup to S3. Daily. |

Analytics data is also reproducible from NATS replay (within stream retention window) - if the analytics store is lost, replay events from JetStream and rebuild.

### Object Storage

S3 (staging/prod) has built-in versioning and cross-region replication. No additional backup needed. Local filesystem is not backed up (recreatable via seed data).

### NATS JetStream

JetStream persists to disk. In prod, NATS should be clustered (3 nodes minimum) for HA. Stream data is replicated across nodes. If a node is lost, the cluster continues.

### Redis

Redis is a cache, not a source of truth. If Redis is lost:
- Budget balances: last known values are in Postgres (periodic flush). DSP falls back to Postgres.
- Frequency caps: reset to zero (users may see an ad one extra time - acceptable).
- Session tokens: users re-login.

No backup needed. Redis can be rebuilt from Postgres state.

### Recovery Targets

| Environment | RTO (recovery time) | RPO (data loss) |
|---|---|---|
| Local | N/A (recreate with seed) | N/A |
| Staging | < 1 hour | < 24 hours |
| Prod | < 15 minutes (failover) | < 1 minute (WAL) |

### High Availability (All Environments)

HA runs everywhere - including locally. Same topology, same failover behaviour. Devs test against a realistic setup from day one.

| Component | Local | Staging | Prod |
|---|---|---|---|
| PostgreSQL | Primary + 1 standby | Primary + 1 standby | Primary + 2 standby (or managed service) |
| NATS | 3-node JetStream cluster | 3-node JetStream cluster | 3-node JetStream cluster |
| Redis | 1 primary + 1 replica + Sentinel | Same | Sentinel per service |
| ClickHouse | N/A (DuckDB embedded) | Optional | Replicated cluster (or managed service) |
| Services | 1 replica per service | 2 replicas | HPA autoscaling (min 2) |

This is heavier on local resources but eliminates "works on my machine" for HA behaviour. Devs can test failover scenarios (kill a Postgres standby, lose a NATS node) locally and know prod will behave the same way.

### Implementation

| Component | Location |
|---|---|
| Postgres backup CronJob | `k8s/cronjobs/backup-postgres/` |
| DuckDB backup CronJob | `k8s/cronjobs/backup-duckdb/` |
| Pre-migration backup | Built into CI/CD pipeline (runs before `cmd/migrate/`) |

---

## Log Aggregation

### Stack: Loki + Promtail + Grafana

We already have Grafana for metrics (Prometheus) and traces (Jaeger). Adding Loki gives us logs in the same UI - one place for everything.

```
Service outputs JSON logs to stdout (slog)
    |
    v
Promtail (K8s DaemonSet, collects container logs from every node)
    |
    v
Loki (stores and indexes logs by labels: service, trace_id, level)
    |
    v
Grafana (query and view logs alongside metrics and traces)
```

### Why Loki

- **Already have Grafana** - logs appear next to metrics and traces in the same dashboards
- **Lightweight** - indexes labels only (service, trace_id, level), not full log content. Much less resource usage than Elasticsearch.
- **Runs locally** - same K8s deployment local and prod
- **Structured JSON** - services already output JSON via slog. Loki ingests natively.
- **Trace ID correlation** - click a trace ID in a Grafana metrics panel → jump directly to matching logs across all services

### Debugging Workflow

1. See error rate spike on Grafana metrics dashboard
2. Click through to logs for that time window
3. Filter by service or log level
4. Find a trace ID in the error log
5. Filter all services by that trace ID - see the full request lifecycle
6. Click through to Jaeger trace for timing waterfall

All in one UI. No switching between tools.

### Log Labels

Promtail automatically adds K8s labels. Services add structured fields via slog:

| Label | Source | Example |
|---|---|---|
| `service` | K8s pod label | `dsp`, `exchange`, `tracker` |
| `namespace` | K8s namespace | `default` |
| `level` | slog log level | `INFO`, `ERROR`, `DEBUG` |
| `trace_id` | Extracted from JSON log field | `abc-123-def` |

### Retention

| Environment | Log retention |
|---|---|
| Local | 3 days (keep disk usage low) |
| Staging | 14 days |
| Prod | 30 days (longer retention in S3-backed Loki) |

### Implementation

| Component | Location |
|---|---|
| Loki deployment | `k8s/base/loki/` |
| Promtail DaemonSet | `k8s/base/promtail/` |
| Grafana log dashboards | `k8s/base/grafana/` (alongside existing metrics dashboards) |
| Log format | `pkg/logger/` - slog JSON output with trace_id, service name |

---

## Testing Strategy

### Layer 1: Unit Tests

Pure logic, no external dependencies. Fast, run on every save.

| Package | What's tested | Example |
|---|---|---|
| `pkg/auction/` | Auction logic (second-price, tie-breaking, floor prices) | "Given 3 bids, second-price returns winner at second-highest price" |
| `pkg/targeting/` | Targeting rule evaluation | "Campaign targeting UK + mobile matches London iPhone request" |
| `pkg/pacing/` | Budget pacing calculations | "Campaign with $100/day at 3pm should have spent ~$62.50" |
| `pkg/pipeline/` | Format detection, validation, field mapping | "CSV with semicolon delimiter auto-detected and parsed" |
| `pkg/middleware/` | Rate limit logic, circuit breaker state machine | "After 5 timeouts, circuit opens. After 30s, half-open." |
| `pkg/openrtb/` | OpenRTB serialisation/deserialisation | "BidRequest serialises to valid OpenRTB 2.6 JSON" |

No database mocks. If a function needs external state, it takes an interface - test with a fake implementation in `pkg/testutil/`.

### Layer 2: Integration Tests (Per-Service)

Each service tested against real dependencies using **testcontainers-go** - spins up real Postgres/Redis/NATS in Docker containers for the test, tears them down after.

| Service | What's tested | Real dependencies |
|---|---|---|
| DSP | Campaign CRUD, bid evaluation, budget decrement | Postgres, Redis |
| Exchange | Auction with multiple DSP responses, timeout handling | NATS |
| Tracker | Event ingestion, writes to NATS, dedup | NATS, Redis |
| Reporting | Consumes from NATS, writes to analytics store, rollup queries | NATS, DuckDB |
| Pipeline | Ingest CSV, validate, normalise, write Parquet | Filesystem, DuckDB |
| Gateway | Auth flow, API endpoint proxying | Postgres |

### Layer 3: End-to-End Tests

Full ad request traced through every service. Runs against a real K8s cluster (local k3s or k3s in CI).

```
Test fires:
  1. Seed minimal profile (1 campaign, 1 placement)
  2. Simulator sends 1 bid request to SSP
  3. Assert: SSP forwarded to Exchange (check trace ID in logs)
  4. Assert: Exchange called DSP, got a bid
  5. Assert: Auction ran, winner selected (check auction event in NATS)
  6. Assert: Ad Server served creative
  7. Assert: Tracker received impression pixel fire
  8. Assert: Reporting consumed event (check analytics store has the row)
  9. Assert: Full trace ID appears across all services
  10. Assert: Budget decremented in Redis and Postgres
```

This is the "zero data slippage" proof. If any step fails, you know exactly where the event dropped.

### Layer 4: Contract Tests

| Type | How | Enforcement |
|---|---|---|
| gRPC contracts | Protobuf definitions | Compile-time. If it compiles, the contract holds. |
| OpenRTB contracts | Round-trip tests in `pkg/openrtb/` validating against the spec | Test-time |
| NATS event contracts | Protobuf-encoded | Compile-time |

### Layer 5: Performance / Load Tests

Using the simulation profiles already defined:

| Seed + Simulation | What it tests | Pass criteria |
|---|---|---|
| `stress` + `burst` | Exchange handles traffic spikes | p99 auction latency < 100ms, zero event loss |
| `stress` + `steady` (10 min) | Sustained load, budget pacing | Budgets deplete correctly, no overspend |
| `standard` + `steady` | Normal operation | All NATS events consumed within 5s, rollups correct |

### Test Data Isolation

- Each integration test gets its own Postgres database (created by testcontainers, unique per test run)
- Each test gets a unique NATS subject prefix so tests don't interfere
- E2E tests use the `minimal` seed profile in an isolated K8s namespace

### What We Never Mock

| Never mock | Why |
|---|---|
| Postgres | SQL behaviour differences between mocks and real DB cause bugs that only show in prod |
| Redis | Atomic operations (`DECRBY`, `INCR`) need real Redis to test correctly |
| NATS | JetStream ack/nak/retry can't be meaningfully mocked |
| Minio/S3 | Use real Minio in tests - same S3 SDK, same behaviour |
| Parquet | Real I/O - mock filesystem hides encoding issues |

### What We Do Fake

| Fake | Why |
|---|---|
| External DSP (in e2e) | We control both sides - simulator acts as the DSP |
| Time | Inject a clock interface for pacing and scheduling tests |

### CI Integration

```
PR pipeline:
  1. go test ./pkg/...                         (unit tests - seconds)
  2. go test ./cmd/... -tags=integration        (integration tests with testcontainers - minutes)
  3. E2E suite on k3s                           (full stack - minutes)
```

Unit tests gate the PR first. Integration tests run in parallel. E2E is the final gate before merge.

---

## Versioning

All data, APIs, and events include version information for future-proofing and safe evolution.

### API Versioning

| Protocol | Versioning approach | Example |
|---|---|---|
| REST (Gateway) | Version in URL path | `/v1/api/campaigns` |
| OpenRTB (bidding) | Version in URL path + OpenRTB spec version in body | `/v1/openrtb/auction` |
| Tracker (pixels) | Version in URL path | `/v1/t/imp?...` |
| gRPC (internal) | Protobuf handles natively (field numbers, `reserved` keyword) | Field added = backwards compatible |

### Event/Data Versioning

Every event and data record carries a `schema_version` field:

```protobuf
message ImpressionEvent {
    int32 schema_version = 1;  // always first field
    string trace_id = 2;
    string campaign_id = 3;
    // ...
}
```

```sql
CREATE TABLE impressions (
    schema_version INT NOT NULL,
    trace_id UUID NOT NULL,
    ...
);
```

| What | Version field | Why |
|---|---|---|
| NATS event messages | `schema_version` in every protobuf message | Consumers know how to deserialise old vs new events during rollout |
| Analytics rows (DuckDB/ClickHouse) | `schema_version` column | Queries and rollups can handle version differences in historical data |
| Parquet/Delta files | `schema_version` in metadata and as column | Pipeline reprocessing handles old file formats correctly |
| Creative assets | Version in object key (`creatives/v2/{id}/banner.html`) | Old creatives still serve while new format rolls out |
| Publisher configs | `config_version` field | Old files reprocessed with the config version that produced them |

### Version Compatibility Rule

Consumers must handle the **current version and one version back**. During a rollout, both versions coexist. Once old data ages out of retention, drop support for the old version.

---

## API Documentation

### Approach

Documentation is generated from the source of truth - no separate wiki or markdown docs that go stale.

| Protocol | Doc tool | Source of truth | Output |
|---|---|---|---|
| gRPC (internal) | Buf | Comments in `pkg/proto/*.proto` | Browsable HTML docs |
| REST (Gateway API) | Swagger UI from OpenAPI spec | `docs/openapi.yaml` | Interactive API explorer at `/docs` |
| OpenRTB (bidding) | Included in OpenAPI spec | `docs/openapi.yaml` | Same Swagger UI |
| Tracker (pixels) | Included in OpenAPI spec | `docs/openapi.yaml` | Same Swagger UI |

### Buf (Protobuf Toolchain)

Replaces raw `protoc` + plugins. Single tool for the full protobuf workflow:

- **Linting** - enforces proto style conventions (field naming, package structure)
- **Code generation** - generates Go code from protos (replaces `make proto`)
- **Breaking change detection** - CI checks that proto changes don't break backwards compatibility
- **Documentation** - generates browsable HTML docs from proto comments

Proto comments become the documentation:

```protobuf
// AuctionService handles real-time auction execution.
service AuctionService {
    // RunAuction initiates an auction for a given placement.
    // The exchange evaluates all eligible DSP bids and returns the winner.
    rpc RunAuction(AuctionRequest) returns (AuctionResponse);
}
```

### OpenAPI (REST Documentation)

A single `docs/openapi.yaml` spec file covers all REST endpoints. Served as an interactive Swagger UI page from the Gateway at `/docs`.

Developers can:
- Browse all available endpoints
- See request/response schemas
- Try endpoints directly from the browser with real requests
- See example payloads

### Implementation

| Component | Location |
|---|---|
| Proto definitions + comments | `pkg/proto/*.proto` |
| Buf config | `buf.yaml`, `buf.gen.yaml` at repo root |
| OpenAPI spec | `docs/openapi.yaml` |
| Swagger UI | Served by Gateway at `/docs` |

---

## Infrastructure Plan

### Architecture Topology

```
                                    Internet
                                       |
                                       v
                              ┌─── Traefik (TLS) ───┐
                              │                      │
                    /v1/api/* │    /v1/t/*    /v1/openrtb/*
                              │       │              │
                              v       v              v
                          [Gateway] [Tracker]  [Exchange cluster]
                              │       │         display│video│dooh│retail│game
                              │       │              │
                    ┌─────────┼───────┼──────────────┤
                    │         │       │              │
                    v         │       v              v
                  [DSP]       │    [NATS JetStream]  [SSP]
                    │         │       │              │
                    v         │       v              v
                  [Redis]     │   [Reporting]    [Ad Server]
                    │         │    (+ billing)       │
                    v         │       │              v
                [Postgres]    │    [DuckDB/CH]   [Minio/S3]
                              │       │
                              │    [SSAI Stitcher]
                              │       │
                              │    [Transcoder]
                              │
                        [Pipeline]
                              │
                        [Minio/S3] (publisher data files)
```

### Cloud Strategy

**Cloud-agnostic by design.** k3s runs on any cloud (or bare metal). No cloud-specific services in the critical path.

| Concern | Approach |
|---|---|
| Compute | k3s cluster on any cloud VMs (AWS EC2, GCP GCE, Azure VMs, Hetzner, DigitalOcean) |
| Block storage | Cloud provider's block storage for PersistentVolumeClaims (EBS, Persistent Disk, etc.) |
| Object storage | Minio locally, S3/GCS/Azure Blob in prod (S3 API compatible) |
| DNS | Any DNS provider (Cloudflare, Route53, etc.) |
| CDN | CloudFront, Fastly, or Cloudflare for video/audio ad segments |
| Load balancer | Cloud provider's LB in front of k3s ingress (or MetalLB for bare metal) |
| Managed databases (optional) | Can swap self-hosted Postgres for RDS, self-hosted ClickHouse for ClickHouse Cloud. Same code, different connection string. |

**Why cloud-agnostic:**
- Avoid vendor lock-in (ad tech margins are thin, cloud costs matter)
- Can start on cheap providers (Hetzner, OVH) and move to AWS/GCP later
- Same k3s everywhere means no cloud-specific K8s quirks
- Could even run on bare metal co-located servers for lowest cost at scale

**Infrastructure as Code:**

Cloud resources (VMs, VPCs, DNS, LB) managed via **Terraform** in an `infra/` directory:

```
infra/
    modules/
        k3s-cluster/     # k3s cluster provisioning (any cloud)
        networking/       # VPC, subnets, security groups
        dns/              # DNS records
        cdn/              # CDN distribution for ad segments
        load-balancer/    # LB in front of k3s
    environments/
        staging/          # staging.tfvars
        prod/             # prod.tfvars
```

Locally: no Terraform needed. Colima handles everything.

### Network Topology

**Production:**

```
┌─────────────── VPC (10.0.0.0/16) ──────────────────────────────┐
│                                                                  │
│  ┌─── Public Subnet (10.0.1.0/24) ───────────────────────────┐ │
│  │  Load Balancer (public IP)                                  │ │
│  │  NAT Gateway (for outbound from private subnet)             │ │
│  └─────────────────────────────────────────────────────────────┘ │
│                                                                  │
│  ┌─── Private Subnet (10.0.10.0/24) ──────────────────────────┐ │
│  │  k3s cluster (all services, all infra)                      │ │
│  │  No direct internet access (NAT only for outbound)          │ │
│  │                                                              │ │
│  │  Services communicate via K8s ClusterIP (internal only)     │ │
│  │  Only Traefik receives external traffic via LB              │ │
│  └─────────────────────────────────────────────────────────────┘ │
│                                                                  │
│  ┌─── Data Subnet (10.0.20.0/24) ─────────────────────────────┐ │
│  │  Postgres primary + standby (or managed RDS)                │ │
│  │  Redis Sentinel cluster                                     │ │
│  │  ClickHouse cluster (if self-hosted)                        │ │
│  │  NATS 3-node cluster                                        │ │
│  └─────────────────────────────────────────────────────────────┘ │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

**Local:** Colima provides a single-node k3s. Everything runs in one network namespace. No VPC needed.

### DNS and Domain Strategy

| Domain | Points to | Purpose |
|---|---|---|
| `adtech.example.com` | Load Balancer | Gateway (dashboard, API) |
| `tracker.example.com` | Load Balancer | Tracker (pixel endpoints, separate domain for cookie isolation) |
| `exchange.example.com` | Load Balancer | Exchange (OpenRTB bidding) |
| `ad-cdn.example.com` | CDN | Ad creative assets (images, video segments) |
| `ssai.example.com` | Load Balancer | SSAI manifest endpoints |
| `*.adtech.example.com` | Load Balancer | Wildcard for future services |

**Local:** All services accessed via `*.localhost` (Traefik handles routing):
- `gateway.localhost` -> Gateway
- `tracker.localhost` -> Tracker
- `grafana.localhost` -> Grafana
- `mailpit.localhost` -> Mailpit
- `traefik.localhost` -> Traefik dashboard

### SSL/TLS

| Environment | Approach |
|---|---|
| Local | Self-signed certs auto-generated by Traefik. Or plain HTTP. |
| Staging | Let's Encrypt via Traefik's built-in ACME (automatic renewal) |
| Prod | Let's Encrypt or managed certificates from cloud provider. Wildcard cert for `*.adtech.example.com`. |

Traefik handles all TLS termination. Services communicate internally over plain HTTP/gRPC (within the k3s cluster network, encrypted by network policy, mTLS added later via Linkerd if needed).

### Resource Requirements Per Service

| Service | CPU request | Memory request | CPU limit | Memory limit | Disk | Notes |
|---|---|---|---|---|---|---|
| **Exchange (per channel)** | 500m | 256MB | 2000m | 1GB | - | CPU-bound (auction computation) |
| **DSP** | 500m | 512MB | 2000m | 2GB | - | Memory for campaign/targeting cache (L1) |
| **SSP** | 250m | 256MB | 1000m | 512MB | - | Lightweight |
| **Ad Server** | 250m | 256MB | 1000m | 1GB | - | Memory for creative cache |
| **Tracker** | 250m | 128MB | 1000m | 512MB | - | High throughput, low memory |
| **Reporting** | 500m | 1GB | 2000m | 4GB | 50GB PVC (DuckDB) | DuckDB needs memory for queries |
| **Gateway** | 250m | 256MB | 1000m | 1GB | - | Proxy, moderate memory for sessions |
| **Pipeline** | 500m | 1GB | 2000m | 4GB | - | Memory for file processing |
| **SSAI Stitcher** | 250m | 256MB | 1000m | 1GB | - | Manifest manipulation is string work |
| **Transcoder (job)** | 2000m | 4GB | 4000m | 8GB | - | CPU/memory intensive per job |
| **Webhooks** | 100m | 128MB | 500m | 256MB | - | Lightweight HTTP dispatcher |
| **Postgres primary** | 1000m | 2GB | 4000m | 8GB | 100GB PVC | Database workload |
| **Postgres standby** | 500m | 1GB | 2000m | 4GB | 100GB PVC | Replication |
| **NATS (per node)** | 250m | 512MB | 1000m | 2GB | 20GB PVC | JetStream persistence |
| **Redis** | 250m | 512MB | 1000m | 2GB | - | In-memory, budget/freq caps |
| **Minio** | 250m | 512MB | 1000m | 2GB | 100GB PVC | Object storage |
| **Prometheus** | 250m | 1GB | 1000m | 4GB | 50GB PVC | Metrics storage |
| **Grafana** | 100m | 256MB | 500m | 512MB | 5GB PVC | Dashboard config |
| **Loki** | 250m | 512MB | 1000m | 2GB | 50GB PVC | Log storage |
| **Jaeger** | 100m | 256MB | 500m | 1GB | 10GB PVC | Trace storage |

**Total resource estimate per environment:**

| Environment | Nodes | Total CPU | Total Memory | Total Disk |
|---|---|---|---|---|
| Local (Colima) | 1 node | 8 CPU cores | 16GB RAM | 100GB disk |
| Staging | 3 nodes | 12 CPU cores | 32GB RAM | 500GB disk |
| Prod (minimum) | 5 nodes | 32 CPU cores | 128GB RAM | 2TB disk |
| Prod (scaled) | 10-20 nodes | 64-128 CPU cores | 256-512GB RAM | 5-10TB disk |

### QPS Targets Per Service

| Service | Local | Staging | Prod (initial) | Prod (scaled) |
|---|---|---|---|---|
| Tracker (events/sec) | 10 | 100 | 5,000 | 50,000 |
| Exchange (auctions/sec) | 10 | 100 | 2,000 | 20,000 |
| DSP (bid evaluations/sec) | 10 | 100 | 10,000 | 100,000 |
| Gateway (API requests/sec) | 10 | 50 | 500 | 5,000 |
| SSAI (manifest requests/sec) | 1 | 10 | 500 | 10,000 |
| Reporting (queries/sec) | 1 | 10 | 50 | 500 |

### Storage Growth Projections

| Store | Growth rate (prod) | 1 month | 1 year | Retention |
|---|---|---|---|---|
| Postgres (transactional) | 1-5 GB/month | 5GB | 60GB | Indefinite (slow growth) |
| Analytics (events) | 10-50 GB/month raw | 50GB | 600GB | Raw: 48h, rollups: tiered |
| Minio/S3 (creatives) | 5-20 GB/month | 20GB | 240GB | Indefinite |
| Minio/S3 (pipeline data) | 10-100 GB/month | 100GB | 1.2TB | Tiered (landing: 30d, normalised: 90d, enriched: 1yr) |
| Minio/S3 (video segments) | 50-200 GB/month | 200GB | 2.4TB | Creative lifetime + 30d after deletion |
| NATS (JetStream) | Bounded by retention | ~20GB max | ~20GB max | Stream retention policies |
| Loki (logs) | 5-20 GB/month | 20GB | - | Local: 3d, staging: 14d, prod: 30d |
| Prometheus (metrics) | 2-5 GB/month | 5GB | - | 30 days |

### Deployment Topology Per Environment

**Local (developer laptop):**
```
Colima (k3s single node)
    All services: 1 replica each (exchange --channel=all)
    All infra: Postgres (primary+standby), NATS (3-node), Redis (primary+replica+sentinel), Minio
    All observability: Prometheus, Grafana, Loki, Promtail, Jaeger
    Dev tools: Mailpit, Traefik dashboard
    Total: ~30 pods on one machine
```

**Staging:**
```
k3s cluster (3 nodes)
    Services: 2 replicas each, exchange per-channel
    Infra: Postgres (primary+standby), NATS (3-node), Redis (shared), Minio or S3
    Observability: full stack
    HPA: enabled (min 1, max 3 per service)
    Backups: daily
```

**Production:**
```
k3s cluster (5-20 nodes, autoscaling node group)
    Services: HPA (min 2, max varies per service)
    Exchange: per-channel deployments, independently scaled
    Infra: Postgres (primary+2 standby or managed), NATS (3-node), Redis (Sentinel per service), ClickHouse (replicated or managed), S3
    Observability: full stack with alerting
    CDN: CloudFront/Fastly for ad segments
    HPA: enabled with production thresholds
    Backups: WAL archiving (continuous), daily base, pre-migration snapshots
    Multi-AZ: nodes spread across availability zones
```

### Cost Estimation

| Environment | Infrastructure | Estimated monthly cost |
|---|---|---|
| Local | Colima on developer laptop | $0 (developer's machine) |
| Staging (3 nodes, cloud) | 3x medium VMs + storage + S3 | $200-500/month |
| Prod minimum (5 nodes) | 5x large VMs + managed Postgres + S3 + CDN | $1,000-3,000/month |
| Prod scaled (20 nodes) | 20x large VMs + managed DBs + S3 + CDN + monitoring | $5,000-15,000/month |

**Cost optimisation:**
- Start on cheap providers (Hetzner: ~$50/month per server vs AWS: ~$200/month)
- Use spot/preemptible instances for non-critical workloads (transcoder, batch fraud, optimise)
- Reserved instances for always-on services (exchange, tracker, DSP)
- Managed databases only when self-hosted operational burden exceeds cost savings
- CDN costs scale with video/audio volume - monitor and cap

### Monitoring and Alerting

**Alert severity levels:**

| Level | Response time | Who gets paged | Example |
|---|---|---|---|
| P1 - Critical | Immediate | On-call engineer (PagerDuty) | No auctions completing, billing pipeline stopped |
| P2 - High | 30 minutes | On-call engineer (Slack + PagerDuty) | Tracker error rate > 5%, NATS consumer lag growing |
| P3 - Medium | 4 hours | Team Slack channel | DuckDB disk > 80%, certificate expiring in 7 days |
| P4 - Low | Next business day | Dashboard only | Single DSP timeout rate elevated, one publisher fill rate dropped |

**Key alerts:**

| Alert | Source | Severity | Condition |
|---|---|---|---|
| Auction latency high | Prometheus | P1 | p99 > 200ms for 5 minutes |
| No impressions | Prometheus | P1 | Zero impressions for 5 minutes |
| NATS consumer lag | Prometheus | P2 | Pending messages > 10,000 for 10 minutes |
| Tracker error rate | Prometheus | P2 | Error rate > 5% for 5 minutes |
| Postgres replication lag | Prometheus | P2 | Lag > 30 seconds |
| Redis memory | Prometheus | P2 | Memory > 90% |
| DuckDB disk usage | Prometheus | P3 | Disk > 80% |
| Certificate expiry | Traefik | P3 | Expires in < 7 days |
| Backup failed | CronJob status | P2 | Backup job failed |
| Pod crash loop | K8s | P2 | Pod restarting > 3 times in 10 minutes |
| HPA at max replicas | K8s | P3 | Service scaled to max for 30+ minutes |
| Budget discrepancy | Reporting verification | P2 | DSP spend != Reporting spend |
| Dead letter queue growing | Prometheus | P2 | DLQ messages > 100 |
| Privacy deletion pending > 72h | Reporting | P2 | Deletion not completed within SLA |

**Alert routing:**

```
Grafana alert fires
    |
    v
P1/P2: PagerDuty -> on-call engineer phone
P2/P3: Slack #adtech-alerts channel
P3/P4: Grafana dashboard annotation only
    |
    v
All alerts: logged in audit trail
```

### Disaster Recovery Procedures

**Scenario 1: Postgres primary fails**
```
1. Automatic: Postgres standby promoted to primary (Patroni/pg_auto_failover)
2. Services reconnect automatically (connection string unchanged, DNS-based failover)
3. Alert: P2 - "Postgres failover occurred"
4. Manual: provision new standby, verify replication
5. RTO: < 30 seconds (automatic failover)
```

**Scenario 2: NATS node fails**
```
1. Automatic: JetStream cluster continues with 2/3 nodes (quorum maintained)
2. Events continue flowing
3. Alert: P2 - "NATS cluster degraded to 2/3 nodes"
4. Manual: provision replacement node, rejoin cluster
5. RTO: 0 (no interruption)
```

**Scenario 3: Full cluster loss**
```
1. Provision new k3s cluster (Terraform apply)
2. Restore Postgres from latest WAL backup (point-in-time)
3. Apply K8s manifests (kustomize overlays/prod)
4. Deploy services (same image tags from last deploy)
5. Verify: run e2e test suite
6. Update DNS to new cluster
7. RTO: < 1 hour
8. RPO: < 1 minute (WAL archiving)
```

**Scenario 4: Redis lost**
```
1. Redis is a cache, not source of truth
2. Restart Redis, it starts empty
3. Budget balances: reload from Postgres (last flush, typically < 10s stale)
4. Frequency caps: reset (users may see an ad one extra time)
5. Session data: users re-login
6. RTO: < 5 minutes
7. RPO: N/A (cache, no data loss concern)
```

### Infrastructure Directory

```
infra/
    terraform/
        modules/
            k3s-cluster/       # Cluster provisioning
            networking/        # VPC, subnets, security groups
            dns/               # DNS records
            cdn/               # CDN distribution
            load-balancer/     # LB configuration
        environments/
            staging/           # staging.tfvars
            prod/              # prod.tfvars
    runbooks/
        postgres-failover.md   # Step-by-step Postgres recovery
        nats-node-replace.md   # NATS node replacement
        full-cluster-restore.md # Full DR procedure
        redis-recovery.md      # Redis rebuild from Postgres
        certificate-renewal.md # Manual cert renewal if ACME fails
        scaling-guide.md       # When and how to add nodes
```

---

## Design Principles

1. **Traceable** - Every ad request gets a trace ID that flows through every service. You can grep logs and see the full lifecycle.
2. **Runnable locally** - `tilt up` and you have the full stack on local k3s via Colima. Live-rebuilds on code change. Same base manifests as staging and production. No cloud dependencies required.
3. **Testable end-to-end** - Integration tests fire a bid request and assert all the way through to the impression event landing in the reporting database.
4. **Transparent** - No magic. No opaque third-party black boxes. Every decision (who won the auction, why a bid was made, where an event went) is visible in code and logs.
5. **Simple first** - Start with the simplest implementation that works. Replace with production-grade components only when needed.
6. **One language** - Go everywhere. Any developer can work on any part of the stack without context switching. Only pivot to another language (e.g. Python for data science) when Go is genuinely the wrong choice.
7. **Zero external shared packages** - Every package the services depend on lives in `pkg/` within this repo. No internal package registries, no cross-repo dependencies. A model is defined once in `pkg/models` and every service imports it directly. This eliminates version drift, duplication, and the "which repo has the latest types?" problem.

---

## Service-to-Service Communication

### Protocol Split

The rule: **gRPC only on hot-path edges where this platform owns BOTH ends;
industry-standard protocols on every boundary an external party can sit on.**
Which transport an edge uses is selected per endpoint by URL scheme in config
(`grpc://` vs `http://`), so rollback/A-B is a config value, not a deploy.

| Communication | Protocol | Why |
|---|---|---|
| Internal hot-path edges we own both ends of: SSP→Exchange auction, Exchange→**our** DSP bid, SSP→Ad Server serve | gRPC (`pkg/grpcx` twins of the HTTP endpoints, ports 81xx) | Multiplexed persistent HTTP/2, binary framing. Low-margin business — every microsecond matters — without touching any external contract. |
| Bidding across any external boundary: Exchange→third-party DSPs (incl. the competitor sims), inbound Prebid, win/loss notices | OpenRTB JSON/HTTP | Industry standard. Never gRPC — interop is the product. The competitor DSPs stay `http://` deliberately so the standard path is exercised in every auction. |
| Frontend dashboard APIs + gateway reverse proxy to services | HTTP/JSON | HTMX requires HTML-over-HTTP; the gateway proxies `/v1/*` over HTTP. |
| Async events (Tracker → Reporting etc.) | NATS JetStream, JSON payloads | Decouples hot path from cold path; exactly-once via Nats-Msg-Id + biz-key dedup. |

### Internal gRPC Twins (implemented — `pkg/grpcx` + `pkg/proto/internalrpc`)

```
SSP ---gRPC---> Exchange ---gRPC---------> DSP (ours)
  \                  \-----OpenRTB/HTTP--> 3rd-party / competitor DSPs
   \--gRPC---> Ad Server   (creative HTML + signed pixel URLs)
Browser ---HTTP pixels---> Tracker ---NATS (JSON)---> Reporting
```

- Each gRPC twin bridges into the **same `http.HandlerFunc`** that serves the
  HTTP endpoint (`grpcx.Bridge`): one code path, two transports, zero drift.
  The envelope carries the exact JSON body plus an HTTP-equivalent status, so
  semantics like the ad server's 429 frequency-cap decline survive.
- Trace context rides gRPC metadata through the same OTel propagator as HTTP
  headers and NATS headers; `trace_id` flows into logs/Jaeger/analytics
  identically on either transport. `adtech_grpc_*` metrics land in each
  service's existing Prometheus registry.
- Ad Server → Tracker is **not** a service call: the ad server bakes signed
  pixel URLs into creative HTML and the browser fires them.

### Service Discovery

Kubernetes internal DNS. HTTP calls use the ClusterIP service name
(e.g. `http://exchange:8081`). gRPC calls use the **headless** `<svc>-grpc`
twin (e.g. `grpc://exchange-grpc:8181`) — `pkg/grpcx` dials `dns:///` with
`round_robin` so RPCs spread across pod IPs instead of pinning one HTTP/2
connection to a single pod via the ClusterIP. No service mesh, no Consul.
Same locally and in prod.

### Proto Definitions

All `.proto` files live in `pkg/proto/`. Generated Go code is committed to the repo so services just import it - no codegen step required during normal development. Proto definitions are the contract between services.

`protoc` generation is a Makefile target (`make proto`) run only when `.proto` files change.

### Event Bus (NATS JetStream)

#### Why NATS

| Concern | NATS | Kafka | Redis Streams |
|---|---|---|---|
| Complexity | Single binary, minimal config | Zookeeper/KRaft, partitions, brokers - heavy | Simple but limited durability |
| Local dev | Tiny footprint, starts in ms | Heavy on a laptop, slow startup | Fine but not purpose-built |
| Go ecosystem | Official Go client, first-class support | Good but more boilerplate | Decent but not native |
| Durability | JetStream adds persistence | Built-in, very strong | Less battle-tested for queues |
| Performance | Extremely fast, low latency | High throughput but higher latency | Fast but single-threaded |

NATS fits the "simple first, runs on a laptop" philosophy. If we outgrow it, the event bus is behind an interface in `pkg/events/` - swap it without changing any service code.

#### Why JetStream (not Core NATS)

Core NATS is fire-and-forget - if a subscriber is down, the message is gone. That's data slippage by design. **JetStream** adds:

- **Message persistence** - events stored on disk until acknowledged
- **Consumer groups** - multiple instances of a service share the load, each message processed once
- **Replay** - reprocess events from a point in time (crucial for fixing bugs in aggregation logic)
- **Acknowledgement** - consumer explicitly acks after processing; if it crashes, the message gets redelivered
- **Retention policies** - keep messages for X days, or until acknowledged, or by size

#### What Happens When a Consumer is Down

Events queue up in the stream. When the consumer comes back, it picks up exactly where it left off. Zero events lost.

```
Tracker publishes -> JetStream Stream (persisted to disk)
                         |
                         |  Reporting is down for 30 min
                         |  Messages accumulate in the stream
                         |
                     Reporting comes back
                         |  Processes all backlogged events
                         |  Acks each one
                         v
                     Stream cleans up acked messages
```

#### Retry Semantics

| Scenario | Behaviour |
|---|---|
| Consumer processes successfully | Ack sent, message removed from pending |
| Consumer crashes mid-processing | No ack, message redelivered after ack timeout (e.g. 30s) |
| Transient error (DB down) | Nak sent, message redelivered with backoff |
| Repeated failure on same message | After N retries (e.g. 5), message moved to dead letter subject |
| Consumer is slow | Max pending messages limit prevents unbounded memory usage |

#### Dead Letter Queue

Messages that fail after N retries get published to `adtech.deadletter.{original_subject}`. These are:

- **Persisted** - never lost
- **Visible** - Grafana alert fires when DLQ grows
- **Inspectable** - dev can examine the failed message and the error
- **Replayable** - fix the bug, drain the DLQ back into the main stream

#### Stream Design

| Stream | Subjects | Retention | Max age |
|---|---|---|---|
| `EVENTS` | `adtech.events.*` | WorkQueue (each message processed once) | 7 days |
| `AUCTIONS` | `adtech.auction.*` | WorkQueue | 7 days |
| `BUDGET` | `adtech.budget.*` | WorkQueue | 24 hours |
| `WEBHOOKS` | `adtech.webhooks.>` | WorkQueue | 7 days |
| `PIPELINE` | `adtech.pipeline.*` | WorkQueue | 7 days |
| `PRIVACY` | `adtech.privacy.*` | WorkQueue | 30 days (deletion audit trail) |
| `BILLING` | `adtech.billing.*` | WorkQueue | 7 days |
| `CAMPAIGN` | `adtech.campaign.*`, `adtech.creative.*` | WorkQueue | 7 days |
| `VIDEO` | `adtech.video.*` | WorkQueue | 7 days |
| `AUDIO` | `adtech.audio.*` | WorkQueue | 7 days |
| `AUDIENCE` | `adtech.audience.*` | WorkQueue | 7 days |
| `CLEANROOM` | `adtech.cleanroom.*` | WorkQueue | 30 days |
| `MARKETPLACE` | `adtech.marketplace.>` | WorkQueue | 30 days |
| `DEADLETTER` | `adtech.deadletter.>` | Limits (by size) | 30 days |

Note: Cache invalidation subjects (`adtech.cache.invalidate.*`) use Core NATS pub/sub, not JetStream streams. They are fire-and-forget by design.

#### Ordering

Events for the same campaign/placement should be processed in order (a click shouldn't be processed before its impression). JetStream supports this via subject-based ordering - partition by key like `adtech.events.impression.{campaign_id}`. Messages within the same subject are delivered in order.

#### Abstraction Layer

Services never touch NATS directly. `pkg/events/` exposes an interface:

```go
type EventBus interface {
    Publish(ctx context.Context, subject string, msg proto.Message) error
    Subscribe(ctx context.Context, subject string, handler EventHandler) error
    Ack(msg *Event) error
    Nak(msg *Event) error
}
```

The JetStream implementation lives in `pkg/events/nats/`. Swapping to Kafka later means adding `pkg/events/kafka/` and changing a config value. No service code changes. Subjects map to topics, consumer groups work the same way, ack/nak semantics are identical.

---

## Seed Data and Simulation

### Seed Data Profiles

Pre-configured scenarios loaded into the database via `cmd/seed/`. Each profile is a YAML file in `profiles/seed/`.

| Profile | Contents | Use case |
|---|---|---|
| `minimal` | 1 advertiser, 1 campaign, 1 creative, 1 publisher, 1 placement | Smoke test, trace a single request end-to-end |
| `standard` | Multiple advertisers, campaigns with different targeting/budgets, several publishers with varied placements, audience segments | General development, testing targeting and auction competition |
| `stress` | Hundreds of campaigns, thousands of placements, overlapping targeting rules | Performance testing, pacing logic, budget contention |
| `demo` | Realistic fake companies, polished creatives, pre-populated reporting history | Demos, screenshots, onboarding new devs |

Usage: `go run ./cmd/seed --profile standard`

### Simulation Profiles

Control how fake traffic behaves via `cmd/simulator/`. Each profile is a YAML file in `profiles/simulation/`.

| Profile | Behaviour | Use case |
|---|---|---|
| `trickle` | 1 req/sec, sequential, verbose logging | Step-by-step debugging, following a single request through the pipeline |
| `steady` | 50-100 req/sec, mixed geo/device/targeting | Day-to-day development, verifying the system under realistic load |
| `burst` | Spike patterns, sudden traffic surges | Testing pacing, budget exhaustion, timeout handling |
| `replay` | Replay a captured sequence of bid requests from a file | Reproducible testing, regression tests |

Usage: `go run ./cmd/simulator --profile steady`

### Combining Profiles

Seed and simulation profiles are independent - pick one of each:

- `minimal` + `trickle` - debugging a single request flow
- `standard` + `steady` - normal development
- `stress` + `burst` - load testing
- `demo` + `steady` - showing the platform to someone

### Tilt Integration

Tilt buttons for common operations:
- **Seed minimal** / **Seed standard** / **Seed demo** - load a seed profile
- **Simulate trickle** / **Simulate steady** - start traffic generation
- **Reset** - wipe the database and reseed

### Custom Profiles

The built-in profiles cover general use cases. Custom profiles can be added as new YAML files in `profiles/` for specific testing scenarios (e.g. a profile that targets a particular edge case in auction logic). No code changes needed - just add a file and reference it by name.

### How Seed and Simulator Work

**Seed (`cmd/seed/`)** writes directly to Postgres using `pkg/store/postgres/`. It bypasses gRPC services because services may not be running yet (seed runs first). It uses the same store layer as services, so validation and multi-tenancy rules are applied.

**Simulator (`cmd/simulator/`)** calls services via their external endpoints - it acts as a real client:
- Sends bid requests to the SSP (which forwards to Exchange via gRPC)
- Fires impression/click pixels at the Tracker (HTTP)
- This ensures simulation exercises the full request path, not just the database

This means seed can run independently, but simulator requires all services to be running.

### Programmable Simulator

The simulator is a **CLI tool** (`cmd/simulator`) with subcommands, a **Go library** (`pkg/simulator`) for tests, and **Tilt resources** for dashboard control.

#### CLI Tool (`cmd/simulator`)

Proper subcommands with flags:

```bash
# Run a simulation profile
simulator run --profile steady --duration 5m

# Run with custom overrides (profile as base, flags override)
simulator run --profile steady --rps 100 --geo UK,DE --format display,native

# Run for a specific number of requests then stop
simulator run --profile trickle --requests 10

# Run with custom exchange/tracker endpoints
simulator run --profile steady --exchange-url http://localhost:8081 --tracker-url http://localhost:8083

# List available profiles
simulator profiles

# Show what a profile contains without running
simulator profiles show steady

# Check if services are reachable before running
simulator check

# Fire a single request (useful for debugging)
simulator single --geo UK --device mobile --format display
```

**Tilt resources** wrap the CLI for dashboard control:

```python
# Tiltfile - simulator as controllable Tilt resources

# One-click simulation buttons
local_resource('sim-trickle',
    cmd='go run ./cmd/simulator run --profile trickle --duration 2m',
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['simulation'],
    auto_init=False)

local_resource('sim-steady',
    cmd='go run ./cmd/simulator run --profile steady --duration 5m',
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['simulation'],
    auto_init=False)

local_resource('sim-burst',
    cmd='go run ./cmd/simulator run --profile burst --duration 1m',
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['simulation'],
    auto_init=False)

local_resource('sim-single',
    cmd='go run ./cmd/simulator single --geo UK --device mobile',
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['simulation'],
    auto_init=False)

# Tilt shows these as buttons in the dashboard sidebar.
# Click to start, click the X to stop. Logs stream in the panel.
```

**Key design:** Tilt resources are just thin wrappers around CLI commands. The CLI is the real tool. Tilt gives it a GUI.

#### Go Library (`pkg/simulator`)

For programmatic use in tests and CI:

**As Go library (in test code):**
```go
sim := simulator.New(simulator.Config{
    RequestsPerSecond: 50,
    Duration:          5 * time.Minute,
    Geo:              []string{"UK", "DE"},
    Devices:          []string{"mobile"},
    AdFormats:        []string{"display", "native"},
    UserPool:         1000,
    IncludeClicks:    true,
    ClickRate:        0.02,
    IncludeConversions: true,
    ConversionRate:   0.005,
    IncludeVideo:     false,
})
sim.Start(ctx)
results := sim.Wait()

assert(results.TotalRequests == 15000)
assert(results.ErrorRate < 0.01)
assert(results.AvgLatencyMs < 50)
```

**As HTTP API (for CI and Tilt buttons):**
```
POST /v1/api/dev/simulator/start
{
    "profile": "steady",
    "duration": "5m",
    "overrides": {
        "requests_per_second": 100,
        "geo": ["UK"],
        "ad_formats": ["video"]
    }
}

GET /v1/api/dev/simulator/status
{"running": true, "requests_sent": 4500, "elapsed": "2m15s", "errors": 0}

POST /v1/api/dev/simulator/stop
{"total_requests": 4500, "stopped_early": true}

GET /v1/api/dev/simulator/results
{
    "total_requests": 15000,
    "successful": 14998,
    "errors": 2,
    "error_rate": 0.00013,
    "avg_latency_ms": 15,
    "p99_latency_ms": 45,
    "impressions_tracked": 14998,
    "clicks_tracked": 302,
    "conversions_tracked": 75,
    "budget_spent": 1250.50,
    "auctions_won": 14998,
    "auctions_lost": 0,
    "no_fill": 2
}
```

**Custom configs for tests:**

Tests define exactly what traffic pattern they need without creating a YAML file:

```go
// A/B test for bid shading - needs specific traffic pattern
abTest := simulator.Config{
    RequestsPerSecond: 50,
    Duration:          5 * time.Minute,
    Geo:              []string{"UK"},
    Devices:          []string{"mobile", "desktop"},
    AdFormats:        []string{"display"},
    UserPool:         5000,           // enough for frequency cap testing
    IncludeClicks:    true,
    ClickRate:        0.015,
    BudgetExhaustion: false,          // keep campaigns running
    Seed:             "standard",     // which seed data to expect
    TraceAll:         true,           // log every trace ID for verification
}

// Performance regression test - push to the limit
perfTest := simulator.Config{
    RequestsPerSecond: 5000,          // stress level
    Duration:          2 * time.Minute,
    Geo:              []string{"UK", "US", "DE", "FR", "JP"},
    Devices:          []string{"mobile", "desktop", "tablet", "ctv"},
    AdFormats:        []string{"display", "native", "video"},
    UserPool:         100000,
    IncludeClicks:    true,
    ClickRate:        0.01,
    IncludeConversions: true,
    ConversionRate:   0.003,
    IncludeVideo:     true,
    VideoCompletionRate: 0.85,
    BudgetExhaustion: true,           // test budget depletion under load
}
```

### k6 Performance Testing

k6 complements the simulator. The simulator tests ad-tech functional correctness; k6 tests raw HTTP performance - finding breaking points, latency under load, and regressions.

**How they work together:**

| Tool | Purpose | What it tests | Runs when |
|---|---|---|---|
| Simulator | Functional ad-tech testing | "Does the full ad flow work correctly?" | Every PR, A/B tests, local dev |
| k6 | Performance/load testing | "Can the tracker handle 50K req/sec?" | Performance PRs, nightly, before major releases |

**k6 test scripts live in the repo:**

```
tests/
    k6/
        tracker-load.js       # Hammer tracker pixel endpoints
        exchange-load.js      # Hammer OpenRTB auction endpoint
        gateway-api-load.js   # Hammer REST API endpoints
        ssai-load.js          # Hammer SSAI manifest endpoints
        mixed-load.js         # Realistic mixed traffic pattern
        thresholds.json       # Pass/fail criteria per test
```

**Example k6 test (tracker load test):**

```javascript
// tests/k6/tracker-load.js
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

export const options = {
    stages: [
        { duration: '30s', target: 100 },    // ramp up to 100 VUs
        { duration: '2m',  target: 1000 },   // ramp to 1000 VUs
        { duration: '1m',  target: 5000 },   // peak at 5000 VUs
        { duration: '30s', target: 0 },      // ramp down
    ],
    thresholds: {
        http_req_duration: ['p(99)<50'],      // 99th percentile < 50ms
        errors: ['rate<0.01'],                // error rate < 1%
        http_req_failed: ['rate<0.01'],
    },
};

export default function () {
    // Fire impression pixel (most common hot path)
    const impRes = http.get(
        `${__ENV.TRACKER_URL}/v1/t/imp?tid=${randomTraceId()}&cid=camp_1&pid=pl_1&sig=test`
    );
    check(impRes, {
        'impression status 200': (r) => r.status === 200,
        'impression latency < 20ms': (r) => r.timings.duration < 20,
    });
    errorRate.add(impRes.status !== 200);

    // Occasionally fire click (2% of requests)
    if (Math.random() < 0.02) {
        const clickRes = http.get(
            `${__ENV.TRACKER_URL}/v1/t/click?tid=${randomTraceId()}&cid=camp_1&pid=pl_1&sig=test&redir=https://example.com`
        );
        check(clickRes, { 'click status 302': (r) => r.status === 302 });
    }
}
```

**Example k6 test (exchange auction):**

```javascript
// tests/k6/exchange-load.js
import http from 'k6/http';
import { check } from 'k6';

export const options = {
    stages: [
        { duration: '30s', target: 50 },
        { duration: '2m',  target: 500 },
        { duration: '1m',  target: 2000 },
        { duration: '30s', target: 0 },
    ],
    thresholds: {
        http_req_duration: ['p(99)<100'],     // auction must complete in 100ms
        http_req_failed: ['rate<0.01'],
    },
};

const bidRequest = JSON.stringify({
    id: 'test-auction',
    imp: [{ id: '1', banner: { w: 300, h: 250 }, bidfloor: 0.50 }],
    site: { domain: 'test-publisher.com', page: 'https://test-publisher.com/sports' },
    device: { ua: 'Mozilla/5.0', ip: '203.0.113.42', geo: { country: 'GBR' } },
    user: { id: 'test-user-' + Math.floor(Math.random() * 10000) },
});

export default function () {
    const res = http.post(
        `${__ENV.EXCHANGE_URL}/v1/openrtb/auction`,
        bidRequest,
        { headers: { 'Content-Type': 'application/json' } }
    );
    check(res, {
        'auction status 200': (r) => r.status === 200,
        'auction has seatbid': (r) => JSON.parse(r.body).seatbid !== undefined,
        'auction latency < 100ms': (r) => r.timings.duration < 100,
    });
}
```

**k6 outputs to Prometheus:**

k6 can push metrics to Prometheus via the Prometheus remote write endpoint, so results appear in the same Grafana dashboards:

```
k6 run --out experimental-prometheus-rw tests/k6/tracker-load.js
```

Grafana dashboard shows k6 metrics alongside service metrics - you see both the load being applied and how the system responds.

**k6 in CI (nightly performance regression):**

```yaml
# .github/workflows/perf-test.yml
name: Performance Regression Test
on:
  schedule:
    - cron: '0 2 * * *'  # nightly at 2am
  pull_request:
    paths: ['cmd/tracker/**', 'cmd/exchange/**', 'pkg/auction/**']

jobs:
  perf-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Start k3s + deploy
        run: make setup-ci && make deploy
      - name: Seed data
        run: make seed profile=stress
      - name: Run k6 tracker load test
        run: k6 run tests/k6/tracker-load.js --env TRACKER_URL=http://tracker.localhost
      - name: Run k6 exchange load test
        run: k6 run tests/k6/exchange-load.js --env EXCHANGE_URL=http://exchange.localhost
      - name: Check thresholds
        run: echo "k6 thresholds enforce pass/fail automatically"
```

**k6 locally (developer finds breaking point):**

```
# "How many requests/sec can MY tracker handle on my laptop?"
k6 run --vus 100 --duration 30s tests/k6/tracker-load.js --env TRACKER_URL=http://tracker.localhost

# Output:
#   ✓ impression status 200
#   ✓ impression latency < 20ms
#
#   http_req_duration...: avg=8.2ms  p(95)=15ms  p(99)=22ms
#   http_reqs...........: 45000     1500/s
#   errors..............: 0.00%
#
# Result: tracker handles 1500 req/sec on laptop. Good.
```

**k6 for testing back-pressure and rate limiting:**

```javascript
// tests/k6/rate-limit-test.js
// Deliberately exceed rate limits to verify they work
export const options = {
    vus: 200,
    duration: '30s',
    thresholds: {
        'http_req_status{status:429}': ['count>100'],  // MUST get 429s (rate limited)
        'http_req_status{status:200}': ['count>1000'],  // but most succeed
    },
};
```

**When to use which:**

| Scenario | Use simulator | Use k6 |
|---|---|---|
| "Does the auction flow work?" | Yes | No |
| "Can the tracker handle 50K req/sec?" | No | Yes |
| "A/B test: is new bid algorithm better?" | Yes | No |
| "Did this code change make auctions slower?" | No | Yes |
| "Does budget depletion work correctly under load?" | Yes | No |
| "What's the breaking point of SSAI?" | No | Yes |
| "Does rate limiting kick in at the right threshold?" | No | Yes |
| "Full e2e trace verification" | Yes | No |

### Implementation

| Component | Location |
|---|---|
| Simulator CLI | `cmd/simulator/` - `--profile`, `--duration`, `--requests` flags |
| Simulator Go library | `pkg/simulator/` - `New()`, `Start()`, `Wait()`, `Results()` |
| Simulator HTTP API | Gateway: `/v1/api/dev/simulator/*` (dev-only routes) |
| Simulation profiles | `profiles/simulation/*.yaml` |
| k6 test scripts | `tests/k6/*.js` |
| k6 thresholds | `tests/k6/thresholds.json` |
| k6 CI workflow | `.github/workflows/perf-test.yml` |
| Performance dashboards | `k8s/base/grafana/` - k6 metrics dashboard |

---

## Container Images

### Shared Dockerfile

All services use a single multi-stage Dockerfile at `build/Dockerfile`. The service to build is passed as a build arg:

```dockerfile
# build/Dockerfile
FROM golang:1.23-alpine AS builder
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app ./cmd/${SERVICE}

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /app /app
ENTRYPOINT ["/app"]
```

Build any service by passing the `SERVICE` arg:
```
docker build --build-arg SERVICE=dsp -f build/Dockerfile -t adtech-dsp .
docker build --build-arg SERVICE=tracker -f build/Dockerfile -t adtech-tracker .
```

### Service-Specific Extensions

Services that need something beyond the standard Go binary get their own Dockerfile:

**Gateway** - embeds HTML templates and static assets:

```dockerfile
# build/Dockerfile.gateway
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /app /app
COPY web/templates /web/templates
COPY web/static /web/static
ENTRYPOINT ["/app"]
```

**Reporting** - requires CGO for the DuckDB Go driver (embedded analytics store):

```dockerfile
# build/Dockerfile.reporting
FROM golang:1.23-alpine AS builder
WORKDIR /src
RUN apk add --no-cache gcc musl-dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /app ./cmd/reporting

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /app /app
ENTRYPOINT ["/app"]
```

**Note:** DuckDB is single-writer. The reporting service must run as a **single replica** when using DuckDB (local/staging). In prod with ClickHouse (client-server, no CGO), the reporting service uses the shared Dockerfile with `CGO_ENABLED=0` and can scale to multiple replicas.

Most services will never need a custom Dockerfile. The shared one handles the common case.

### Tilt Integration

Tilt uses the same Dockerfiles. In the Tiltfile, each service is defined once:

```python
# Tiltfile (simplified)

# Apply all K8s manifests once
k8s_yaml(kustomize('k8s/overlays/local'))

# Standard services (shared Dockerfile, CGO_ENABLED=0)
services = ['dsp', 'ssp', 'adserver', 'tracker', 'pipeline', 'webhooks', 'ssai']

for svc in services:
    docker_build(
        'adtech-' + svc,
        '.',
        dockerfile='build/Dockerfile',
        build_args={'SERVICE': svc},
        only=['cmd/' + svc, 'pkg/', 'go.mod', 'go.sum'],
    )

# Exchange (multiple channel instances in prod, single --channel=all locally)
docker_build(
    'adtech-exchange',
    '.',
    dockerfile='build/Dockerfile',
    build_args={'SERVICE': 'exchange'},
    only=['cmd/exchange', 'pkg/', 'go.mod', 'go.sum'],
)

# Gateway (embeds web assets)
docker_build(
    'adtech-gateway',
    '.',
    dockerfile='build/Dockerfile.gateway',
    only=['cmd/gateway', 'pkg/', 'web/', 'go.mod', 'go.sum'],
)

# Reporting (CGO_ENABLED=1 for DuckDB)
docker_build(
    'adtech-reporting',
    '.',
    dockerfile='build/Dockerfile.reporting',
    only=['cmd/reporting', 'pkg/', 'go.mod', 'go.sum'],
)

# Transcoder (requires FFmpeg for video/audio transcoding)
docker_build(
    'adtech-transcoder',
    '.',
    dockerfile='build/Dockerfile.transcoder',
    only=['cmd/transcoder', 'pkg/', 'go.mod', 'go.sum'],
)
```

The `only` parameter means Tilt only triggers a rebuild when relevant files change - a change to `cmd/dsp/` won't rebuild the tracker image.

---

## CI/CD

### Local Development

Tilt handles the full loop locally. When a dev saves a Go file, Tilt detects the change, rebuilds the binary, builds the container image locally (no push to a registry), and hot-deploys it into the local k3s cluster. This keeps the feedback loop to seconds, not minutes.

- Images are built locally using the Colima Docker daemon - never pushed anywhere
- Tilt only rebuilds the service that changed (unless `pkg/` changed, then it rebuilds dependents)
- `tilt up` is the only command a dev needs

### CI Pipeline (GitHub Actions)

```
PR opened / updated
  |
  +-> Lint (golangci-lint)
  +-> Test (go test ./... - all packages, always)
  +-> Build affected images (path-based change detection)
  +-> Integration tests (spin up k3s in CI, apply local overlay, run e2e suite)
  |
Merge to main
  |
  +-> Build affected images (tagged with git SHA)
  +-> Push to GitHub Container Registry (ghcr.io)
  +-> Auto-deploy to staging (kustomize overlays/staging, update image tags)
  |
Promote to prod (manual)
  |
  +-> Dev triggers workflow_dispatch or approves environment gate
  +-> Apply kustomize overlays/prod with the same tested image tag
```

### Change Detection Rules

| Path changed | Tests | Image rebuild | Deploy |
|---|---|---|---|
| `cmd/dsp/**` | All | DSP only | DSP only |
| `cmd/tracker/**` | All | Tracker only | Tracker only |
| `pkg/**` | All | All services | All services |
| `k8s/**` | All | None | Re-apply manifests only |
| `web/**` | All | Gateway only (serves UI) | Gateway only |
| `docs/**` | Skip | Skip | Skip |

### Image Tagging

All images are tagged with the git SHA of the commit that built them. No semver, no `latest`. Every running container maps to an exact commit. Tag format: `ghcr.io/<org>/adtech-<service>:<git-sha>`.

### Key Decisions

- **Single registry:** GitHub Container Registry (ghcr.io) - tied to the repo, no extra accounts
- **No GitOps controller yet:** GitHub Actions applies manifests directly via `kubectl`. Add Argo CD later if needed
- **Prod is always manual:** No auto-deploy to production. A human approves every prod release
- **k3s in CI:** Integration tests run on real K8s in the GitHub Actions runner, same manifests as local dev
- **Local builds stay local:** During development, images are built and deployed within Colima - nothing leaves the machine

### GitHub Actions Workflow Files

All workflow files live in `.github/workflows/`. Here's each file and what it does:

#### 1. `ci.yml` - PR Pipeline (every PR)

```
Trigger: PR opened / updated / synchronized

Jobs (parallel where possible):

┌─ lint ──────────────────────────────────────────┐
│  golangci-lint run ./...                        │
│  buf lint (proto files)                         │
│  buf breaking (proto backwards compatibility)   │
└─────────────────────────────────────────────────┘

┌─ test ──────────────────────────────────────────┐
│  go test ./pkg/... (unit tests)                 │
│  go test ./cmd/... -tags=integration            │
│    (testcontainers: real Postgres/Redis/NATS)   │
└─────────────────────────────────────────────────┘

┌─ build ─────────────────────────────────────────┐
│  Detect changed paths -> build affected images  │
│  Verify Dockerfiles build successfully          │
└─────────────────────────────────────────────────┘

┌─ e2e (depends on build) ────────────────────────┐
│  Start k3s in CI runner                         │
│  Apply kustomize overlays/local                 │
│  Run migrations                                 │
│  Seed minimal profile                           │
│  Run e2e test suite (trace single request)      │
│  Verify: route tests (all endpoints have gRPC)  │
└─────────────────────────────────────────────────┘

┌─ ab-test (conditional) ─────────────────────────┐
│  Only runs if: pkg/auction/**, pkg/targeting/**, │
│    pkg/pacing/**, cmd/dsp/**, cmd/exchange/**    │
│  Deploy stable (from main) + canary (from PR)   │
│  Run simulation (steady, 5min)                  │
│  Compare metrics, assert pass criteria          │
│  Report results as PR comment                   │
└─────────────────────────────────────────────────┘

Summary: posted as PR check with pass/fail per job
```

#### 2. `nightly.yml` - Full Nightly Pipeline

```
Trigger: cron '0 2 * * *' (2am UTC daily)

Jobs (sequential):

1. FULL BUILD
   - Build ALL service images (not just changed)
   - Push to ghcr.io with tag: nightly-{date}

2. FULL TEST SUITE
   - Start k3s cluster in CI
   - Deploy all services
   - Run migrations
   - Seed standard profile
   - Unit tests: go test ./...
   - Integration tests: go test ./... -tags=integration
   - E2E tests: full ad request trace
   - Route tests: all endpoints mapped

3. PERFORMANCE TESTS
   - Seed stress profile
   - k6 tracker load test (ramp to 5000 VUs)
   - k6 exchange load test (ramp to 2000 VUs)
   - k6 gateway API test (ramp to 500 VUs)
   - Assert all thresholds pass

4. CHAOS TESTS
   - Seed standard profile
   - Run simulation (steady, 5min) + chaos:
     a. redis_failure scenario
     b. nats_partition scenario
     c. pod_kill scenario
   - Verify: 0% event loss, recovery < 30s, no double-billing

5. A/B REGRESSION
   - Deploy current main as stable
   - Deploy previous release as canary
   - Run simulation, verify no regression from previous release

6. SECURITY SCAN
   - go vuln check (dependency vulnerabilities)
   - Docker image scan (Trivy or Grype)
   - Buf breaking change check against last release

7. SUMMARY REPORT
   - Aggregate all results into a summary
   - Post to Slack channel
   - Store in GitHub Actions artifacts
   - If any failures: create GitHub Issue automatically

Summary format:
┌─────────────────────────────────────────────────┐
│  Nightly Build Report - 2026-05-28              │
│                                                  │
│  Build:       ✓ All 21 binaries built           │
│  Unit tests:  ✓ 1,247 passed, 0 failed          │
│  Integration: ✓ 89 passed, 0 failed             │
│  E2E:         ✓ Full trace verified             │
│  Routes:      ✓ All 130+ endpoints mapped       │
│                                                  │
│  Performance:                                    │
│    Tracker:   ✓ p99=22ms (threshold <50ms)      │
│    Exchange:  ✓ p99=85ms (threshold <100ms)     │
│    Gateway:   ✓ p99=45ms (threshold <100ms)     │
│                                                  │
│  Chaos:                                          │
│    Redis:     ✓ Recovery 8s, 0 events lost       │
│    NATS:      ✓ Recovery 12s, 0 events lost      │
│    Pod kill:  ✓ Recovery 5s, 0 events lost       │
│                                                  │
│  Security:                                       │
│    Vulns:     ✓ 0 critical, 2 low               │
│    Images:    ✓ No CVEs above medium             │
│                                                  │
│  Overall: PASS                                   │
└─────────────────────────────────────────────────┘
```

#### 3. `deploy-staging.yml` - Deploy to Staging (merge to main)

```
Trigger: push to main branch

Jobs:

1. BUILD + PUSH
   - Detect changed paths
   - Build affected images (git SHA tag)
   - Push to ghcr.io

2. PRE-DEPLOY BACKUP
   - pg_dump of staging Postgres

3. MIGRATE
   - Run goose up against staging Postgres
   - If migration fails: stop, alert, don't deploy

4. DEPLOY
   - kustomize build overlays/staging
   - Update image tags to new git SHA
   - kubectl apply
   - Wait for rollout complete

5. SMOKE TEST
   - Seed minimal profile (if fresh)
   - Fire single ad request, verify full trace
   - Check all health endpoints return 200

6. NOTIFY
   - Post to Slack: "Staging deployed: {git SHA}, {PR title}"
   - Record in deployment ledger
   - Push Grafana annotation
```

#### 4. `deploy-prod.yml` - Deploy to Production (manual)

```
Trigger: workflow_dispatch (manual) with required inputs:
  - git_sha: which image tag to deploy (must exist in ghcr.io)
  - confirm: "yes" (typed confirmation)

Jobs:

1. VALIDATE
   - Verify image tag exists in ghcr.io
   - Verify this SHA passed nightly tests
   - Verify this SHA is currently running in staging

2. PRE-DEPLOY BACKUP
   - pg_dump of prod Postgres
   - Snapshot DuckDB/ClickHouse

3. MIGRATE
   - Run goose up against prod Postgres
   - If fails: stop, alert, don't deploy

4. DEPLOY (with approval gate)
   - GitHub environment protection: requires manual approval
   - kustomize build overlays/prod
   - Update image tags
   - kubectl apply
   - Wait for rollout complete

5. VERIFY
   - Health check all services
   - Fire synthetic ad request, verify trace
   - Check Prometheus: error rate not spiking
   - Check deployment ledger: record this deploy

6. NOTIFY
   - Post to Slack: "PROD deployed: {git SHA}, {deployer}"
   - Push Grafana annotation (appears on all dashboards)
   - Record in deployment ledger
```

#### 5. `perf-test.yml` - Performance Regression (nightly + on-demand)

```
Trigger:
  - schedule: nightly
  - pull_request: paths ['cmd/tracker/**', 'cmd/exchange/**', 'pkg/auction/**']

Jobs:
  - Start k3s, deploy, seed stress profile
  - Run k6 tests (tracker, exchange, gateway, SSAI)
  - Assert thresholds
  - Post results as PR comment (if PR) or Slack (if nightly)
```

#### 6. `chaos-test.yml` - Chaos Regression (nightly)

```
Trigger: schedule nightly (after perf-test)

Jobs:
  - Start k3s, deploy, seed standard profile
  - Run simulation + chaos scenarios (redis, nats, pod kill, cascade)
  - Verify pass criteria (0% event loss, recovery time, no double-billing)
  - Post results to Slack
```

### Workflow File Directory

```
.github/
    workflows/
        ci.yml              # PR pipeline (lint, test, build, e2e, conditional A/B)
        nightly.yml         # Full nightly (build, test, perf, chaos, security, summary)
        deploy-staging.yml  # Auto-deploy to staging on merge to main
        deploy-prod.yml     # Manual prod deploy with approval gate
        perf-test.yml       # k6 performance regression
        chaos-test.yml      # Chaos resilience regression
```

Added to monorepo directory structure.

---

## Multi-Tenancy Isolation

### The Risk

Multiple advertisers and publishers share the same platform. Without proper isolation, Advertiser A could see Advertiser B's campaigns, budgets, or performance data. One missed WHERE clause in a query = data leak.

### Defence in Depth: Two Layers

#### Layer 1: Application-Level (Code)

Every query includes `WHERE account_id = ?`. The account ID comes from the authenticated JWT, extracted by middleware and passed through the request context. Services never trust a client-supplied account ID.

```go
// pkg/store/postgres/campaigns.go
func (s *Store) ListCampaigns(ctx context.Context) ([]Campaign, error) {
    accountID := auth.AccountIDFromContext(ctx)  // extracted from JWT by middleware
    return s.db.Query("SELECT * FROM campaigns WHERE account_id = $1", accountID)
}
```

Every store method follows this pattern. There is no `ListAllCampaigns()` without an account filter - it doesn't exist in the API.

#### Layer 2: Postgres Row-Level Security (RLS)

Database-level enforcement as a safety net. Even if code has a bug and forgets the WHERE clause, Postgres blocks access to other tenants' data.

```sql
-- Enable RLS on all tenant-scoped tables
ALTER TABLE campaigns ENABLE ROW LEVEL SECURITY;
ALTER TABLE creatives ENABLE ROW LEVEL SECURITY;
ALTER TABLE placements ENABLE ROW LEVEL SECURITY;

-- Policy: users can only see rows matching their account_id
CREATE POLICY tenant_isolation ON campaigns
    USING (account_id = current_setting('app.current_account_id')::UUID);
```

The service sets `app.current_account_id` on every database connection from the JWT context before executing queries:

```sql
SET LOCAL app.current_account_id = 'account-uuid-here';
```

If code somehow misses the WHERE clause, RLS silently filters the results. No data leaks.

### What's Tenant-Scoped

| Table | Scoped by | Notes |
|---|---|---|
| campaigns | account_id (advertiser) | Advertiser only sees their campaigns |
| creatives | account_id (advertiser) | Advertiser only sees their creatives |
| targeting | account_id (advertiser) | Via campaign ownership |
| placements | account_id (publisher) | Publisher only sees their placements |
| audience_segments | account_id | Segments are per-account |
| invoices | account_id (advertiser) | Advertiser only sees their invoices |
| payouts | account_id (publisher) | Publisher only sees their payouts |
| saved_reports | account_id | Reports are per-account |
| audit_log | account_id | Filtered per account in dashboard (platform admins see all) |

### What's NOT Tenant-Scoped

| Data | Why |
|---|---|
| Analytics events (impressions, clicks) | Aggregated across tenants for platform reporting. Tenant filtering applied at query time via dimensions. |
| Auction logs | Cross-tenant by nature (multiple advertisers bid on same placement). Access restricted to platform admins. |
| Live config | Platform-wide settings, not per-tenant. |
| Fraud rules / blocklists | Platform-wide. |

### Analytics Isolation

The analytics store (DuckDB/ClickHouse) doesn't support RLS. Tenant isolation is enforced at the application level in `pkg/reporting/builder.go`:

- Every report query automatically injects a `campaign_id IN (SELECT id FROM campaigns WHERE account_id = ?)` filter for advertisers
- Every report query automatically injects a `placement_id IN (SELECT id FROM placements WHERE account_id = ?)` filter for publishers
- The report builder API never exposes raw SQL - users pick dimensions/metrics/filters and the query is constructed server-side with tenant filtering baked in

### API Isolation

Gateway middleware extracts `account_id` from JWT and injects it into gRPC metadata. Every downstream service receives the account context automatically.

```
Request with JWT -> Gateway middleware extracts account_id
    |
    v
gRPC metadata: account_id = "abc-123"
    |
    v
DSP/SSP/Reporting: every store call filters by account_id
    + Postgres RLS as safety net
```

### Testing

Multi-tenancy isolation is tested explicitly:
- Integration tests create two accounts, seed data for both, and verify that queries from Account A never return Account B's data
- Tests attempt to access resources across accounts and assert 403/empty results
- RLS is tested by intentionally omitting WHERE clauses in test queries and verifying Postgres still filters correctly

### Implementation

| Component | Location |
|---|---|
| Account context middleware | `pkg/middleware/tenant.go` - extracts account_id from JWT, sets on context and gRPC metadata |
| RLS setup | `migrations/` - RLS policies created alongside table definitions |
| Store enforcement | `pkg/store/postgres/` - every query method reads account_id from context |
| Analytics filtering | `pkg/reporting/builder.go` - auto-injects tenant filter into all report queries |
| Tenant isolation tests | `pkg/testutil/tenant_test.go` - cross-account access tests |

---

## Developer Onboarding

### Prerequisites

| Tool | Purpose |
|---|---|
| Go 1.23+ | Language runtime |
| Colima | Runs K8s locally via k3s |
| kubectl | K8s CLI |
| Helm + Rancher Desktop | Dev orchestration (Tilt retired 2026-07-18) |
| Buf | Protobuf toolchain |

### Getting Started

```
git clone <repo>
cd ad-tech-mono
make setup       # installs prerequisites via brew, starts Colima + k3s
make stack-up    # builds all images, helm-installs the stack, runs migrations
```

Watch pods with `kubectl get pods -n adtech -w`; per-service redeploy is `make deploy SVC=<name>`. (An ops console UI is planned — see the outstanding-work ledger.)

### First Run

1. `tilt up` - full stack running
2. Click **Seed standard** in Tilt dashboard - loads test data
3. Click **Simulate trickle** - one request per second flowing through the system
4. Open Grafana - watch metrics appear
5. Grab a trace ID from the logs - grep it across services to see the full lifecycle

### Makefile Targets

| Command | What it does |
|---|---|
| `make setup` | Install prerequisites, start Colima + k3s |
| `make proto` | Regenerate Go code from proto files (via Buf) |
| `make test` | Run unit tests |
| `make test-integration` | Run integration tests (needs Docker for testcontainers) |
| `make lint` | Run golangci-lint |
| `make seed` | Seed database with standard profile |
| `make simulate` | Start steady simulation |
| `make reset` | Wipe database and reseed |

---

## Build Plan & Status (single source of truth)

> **This section is the continuous plan.** The 123 numbered steps are the
> backlog; each phase carries a live status glyph; the **Build Status &
> Outstanding Work** ledger at the end of this section is the one place that
> tracks "plan vs reality" (it folds in the former `MOCK_AUDIT.md` and
> `OUTSTANDING_WORK.md`, both deleted). Reconciled against disk 2026-07-01;
> re-reconciled 2026-08-09 (product-gaps session).
>
> **Status glyphs:** ✅ done · ⚠️ partial (library real but no runnable binary,
> or serves but hardcoded) · ❌ empty shell / pure stub · ⬜ not started.
> **Done = the named `tests/e2e/*_test.go` case flips from `t.Skip` to a passing
> assertion**, where one exists.

### ✅ Phase 1: Foundation (COMPLETE)
1-11. Go module, clock, protos, K8s/Tilt, migrations, shared packages (logger, health, lifecycle, config), event bus + idempotent consumer, Postgres store + RLS, cache (L1+L2), currency, auth/RBAC.

### ✅ Phase 2: Core Ad Serving + Dev Tools (First Ad Served)
12. Build the Exchange - first-price auction engine, 5 strategies, deal priority, competitive separation, OpenRTB (win + loss notices)
13. Build the DSP - IO/line item hierarchy, targeting with exclusions, bid modifiers, bid shading, pacing (even/ASAP/front-loaded), budget (reserve/settle)
14. Build the SSP - placements, bid request generation (site + app + regs), quality controls, deals (PMP/PG/preferred), floor prices
15. Build the Ad Server - display + native serving, macros (26), third-party pixels, frequency capping, DCO, sequential messaging
16. Build the Tracker - impression/click/conversion/viewability endpoints, NATS publishing, real-time fraud checks, data collection pixels
17. Build the Gateway - auth (JWT/RBAC), API proxying, HTMX dashboard (role-gated), Swagger UI
18. Implement loss notification processing + win-rate feedback loop
19. Implement contextual targeting classification engine
20. Wire up end-to-end: SSP -> Exchange -> DSP -> Ad Server -> Tracker -> Reporting
21. Implement day boundary job - daily budget resets, flight automation
22. **Dev tools (Phase 2):** Trace Explorer (basic - Grafana dashboard + HTMX live trace)
23. **Dev tools (Phase 2):** Publisher Simulator - `minimal`, `news_site`, `ecommerce` templates with debug overlay
24. **Dev tools (Phase 2):** Seed profiles (minimal + standard), `trickle` + `steady` simulation profiles
25. **Dev tools (Phase 2):** First e2e test: trace a single ad request through all services
26. Set up observability stack (Prometheus, Grafana dashboards, Loki, Promtail, Jaeger)

### ✅ Phase 3: Data and Reporting + Dashboards
27. Implement `pkg/store/analytics/` (DuckDB + ClickHouse interface) with dual-write
28. Build the Reporting service (unified with billing) - NATS consumer, analytics + billing writes atomically
29. Implement universal rollup framework (`pkg/store/rollup/`)
30. Build custom report builder (`pkg/reporting/builder.go`) with reach/frequency (HyperLogLog)
31. Implement reach/frequency forecasting - campaign planning tools
32. Build data pipeline (`cmd/pipeline/`) - ingest, validate, normalise, enrich, schema drift
33. Implement Parquet/Delta Lake storage with analytics schema evolution strategy
34. **Dev tools (Phase 3):** Grafana dashboards (metrics + logs + deployment annotations)
35. **Dev tools (Phase 3):** Trace Explorer enhanced - add reporting/billing events to trace view

### ✅ Phase 4: Billing and Finance
36. Billing in reporting service - AuctionWinEvent consumer, reserve/settle for CPM/CPC/CPA/vCPM/CPCV
37. Double-entry accounting ledger (`pkg/billing/ledger.go`)
38. View-through conversion attribution with configurable windows
39. Variable margin/revenue share models - fixed, tiered, guaranteed minimum, deal-type
40. Reconciliation verification (simplified - single consumer)
41. Invoice and payout generation with multi-currency
42. Billing dashboard views
43. **Dev tools (Phase 4):** Trace Explorer - add budget impact panel ("was $100, cost $3, now $97")
44. **Dev tools (Phase 4):** Publisher Simulator - billing debug in overlay (cost per impression, billing model)

### ✅ Phase 5: Identity, Privacy, and Audience  — step 50 shipped 2026-07-05; step 45 closed by the profile-store epic (identity graph + clusters wired into serving AND attribution — see ledger)
45. `pkg/identity/` - platform ID, identity graph, cross-device linking
46. Unified audience store (`pkg/audience/store/`) - Redis + Postgres, access-controlled
47. Audience management - segments, lookalike audiences, composite segments, retargeting builders
48. `web/static/adtech.js` - publisher ad tag SDK (platform ID, user data, viewability, native, contextual)
49. `pkg/privacy/` - consent checking, opt-out system (3 levels), deletion propagation
50. `cmd/privacy-delete/` and `cmd/privacy-verify/` - deletion + verification jobs
51. Privacy compliance dashboard
52. Inventory quality scoring (`pkg/targeting/quality.go`)
53. **Dev tools (Phase 5):** Publisher Simulator - add `mobile_app` template (IDFA/GAID handling)
54. **Dev tools (Phase 5):** Publisher Simulator - add user profile switching, consent toggle, geo switching
55. **Dev tools (Phase 5):** Audience debug in overlay (show user's segments, access-filtered per DSP)

### ⚠️ Phase 6: Fraud and Quality  — real-time done; step 61 shipped 2026-07-05, step 58 outstanding (see ledger)
56. Real-time fraud checks (`pkg/fraud/realtime.go`) - bot detection, IP blocklist, rate limiting
57. Fraud scoring (`pkg/fraud/scoring.go`)
58. Batch fraud detection (`cmd/fraud/` CronJob)
59. ads.txt / app-ads.txt crawler + verification
60. Publisher quality controls and creative review workflow
61. Serve `sellers.json` from Gateway
62. **Dev tools (Phase 6):** Publisher Simulator - fraud score in debug overlay per impression

### ✅ Phase 7: Optimisation  — libs wired in-process; step 63's one real gap (routing warm-start) shipped in cmd/exchange (warmstart.go boot seed + routingsync.go cross-replica reseed); a standalone cmd/optimise binary is not needed (see ledger)
63. Bid optimisation pipeline (`cmd/optimise/`) - shading curve updates, placement scoring
64. Creative performance - multi-arm bandit, DCO component-level optimisation
65. Auto-optimisation - budget reallocation across line items within IO
66. Smart routing phases 2 and 3 in Exchange
67. Campaign recommendations engine
68. Set up Python training environment (`python/`)
69. **Dev tools (Phase 7):** Campaign recommendations in dashboard UI

### ⚠️ Phase 8: Testing, Ops, and Infrastructure  — steps 72, 76, 77 shipped 2026-07-05; 70–71 thin (see ledger)
70. Programmable simulator (`pkg/simulator/`) - Go library + HTTP API + CLI
71. k6 performance test scripts (`tests/k6/`)
72. Chaos testing framework (`pkg/chaos/`) + chaos profiles
73. Operations UI - A/B testing, canary management, deployment overview
74. Local canary/A/B testing workflow with Tilt buttons
75. Deployment ledger + Grafana annotations
76. Webhooks dispatcher (`cmd/webhooks/`)
77. Email sending (`pkg/email/`) + Mailpit
78. Configuration management UI (live config dashboard)
79. Audit log dashboard
80. Stress + burst simulation profiles

### 🟨 Phase 9: Video, Audio, and Extended Channels (each with simulator template)  — core DONE via the simulator/SSAI epic (merged 06e5734); extended-channel MVPs SHIPPED 2026-08-02/03
Status audited 2026-07-15, re-audited 2026-08-09 — the video/audio CORE (81–88)
shipped with the production-like-simulator epic; household targeting + freq caps
shipped 2026-07-15/16; DOOH / retail / in-game MVPs shipped 2026-08-02/03 (all 5
auction strategies real, TestNoStubbedStrategiesRemain pins it; each channel's
"Built (MVP)" block in its own section documents ship-vs-planned). Remaining:
channel-specific simulator template polish, radio-vs-podcast, SSAI extras.
81. ✅ OpenRTB `video` and `audio` objects in bid requests (pkg/openrtb; SSP builds, DSP evaluates, incl. pod fields)
82. ✅ VAST 4.2 XML generation + DAAST for audio (pkg/vast; publisher-adserver vast.go/audio.go)
83. ✅ VMAP with pre-roll/mid-roll/post-roll scheduling (pkg/vmap; /v1/pubad/video/vmap)
84. ✅ Ad pod auction logic (`pkg/auction/pods.go` — variable + fixed-slot, competitive separation, ShortFill)
85. ✅ Video transcoder service (`cmd/transcoder/` — real ffmpeg, ABR ladders, S3 cache; integration tests thin)
86. 🟨 **+ Publisher Simulator:** `video_page` — covered by the generic Video tab (VAST/VMAP/pod); quartile-debug polish open
87. ✅ SSAI manifest manipulator (`cmd/ssai/` — HLS AND DASH, session manager, server-side beacons, multi-rung ABR)
88. 🟨 SSAI production - slate ✅, ABR ✅; CDN failover / bumpers / DVR-time-shift NOT built
89. ⬜ **+ Publisher Simulator:** `live_stream` template (SSAI stitched, session debug) — sim is VOD-only today
90. ✅ CTV bid request handling (device=ctv, pod context end-to-end) + household targeting (SSP derives hh: id from salted client-IP HMAC — identity.HouseholdID — carried as a user.eids entry [source adtech.household]; household audience segments are ordinary audience members keyed by the hh: id, consent-gated at the DSP; identity graph links user↔household via SourceHousehold). Household FREQUENCY caps ✅ (SSP forwards hh: id on ServeRequest.household_id; ad server enforces the campaign cap per household in addition to per user — co-viewing devices on one IP share the counter). The cap applies across ALL formats: display is capped inside the ad server's render call, and for video/native/audio (rendered by the publisher-adserver, not the ad server) the SSP makes a cap-only ad-server call before returning the winner (ServeRequest.channel set → the ad server runs the cap and returns allowed/429 without rendering). Fail-open on a cap-service error so a blip can't black out non-display serving.
91. ⬜ **+ Publisher Simulator:** `ctv_player` template (full-screen, household signals)
92. 🟨 Audio: podcast insertion ✅ (Feed=2, SSAI audio); streaming-radio distinction ⬜ (Feed=3 treated as podcast)
93. ⬜ **+ Publisher Simulator:** `podcast` template (DAAST + waveform) + `radio_stream` template
94. 🟨 DOOH **MVP SHIPPED 2026-08-02**: TimeSlot strategy real (single-winner screen play), proof-of-play beacon with audience multiplier (`ch=dooh&mult=N` → one impressions row, `impression_qty=N`, full play cost; reporting COUNT=SUM(impression_qty)); screen management / weather / venue controls open — see the DOOH "Built (MVP)" block
95. 🟨 **+ Publisher Simulator:** DOOH tab shipped (e0dc800); `billboard` polish (rotation, weather, venue controls) open
96. 🟨 Retail media **MVP SHIPPED 2026-08-03**: RelevanceWeightedStrategy real (relevance×bid slate, GSP pricing, MinRelevance floor, per-product category via line_items.product_category, multi-slot per-surface billing); catalog sync + keyword bidding open — see the retail "Built (MVP)" block
97. 🟨 **+ Publisher Simulator:** retail tab shipped (e0dc800); `retail_search` polish (relevance-score display) open
98. 🟨 In-game **MVP SHIPPED 2026-08-03**: BatchStrategy real (batch scene auction, competitive separation — one advertiser + one category per scene, per-surface sub-trace billing); rewarded verification + intrinsic viewability open — see the in-game "Built (MVP)" block
99. 🟨 **+ Publisher Simulator:** in-game tab shipped (e0dc800); `game_scene` polish (rewarded prompt) open

### 🟨 Phase 10: Clean Rooms and Data Marketplace  — marketplace SHIPPED 2026-08-10; clean-room engine + bartering deferred
100. ⬜ Clean room computation engine (`pkg/cleanroom/`) — deferred (dir is .gitkeep-only; the shipped clean-room-LITE expansion estimate lives in `pkg/marketplace` + gateway)
101. ⬜ Clean room isolated job runner (`cmd/cleanroom/`) — deferred (.gitkeep-only)
102. ✅ Data marketplace - listings, expansion estimates, purchase flow — **COMPLETE 2026-08-10**: listings+discovery (mig 088), purchase+activation grants (mig 089), expansion estimates (clean-room-lite, min-agg-100 privacy floor); gateway list/browse/purchase/grants/estimate + portal Marketplace tab + RBAC marketplace:read/list/buy; PUBLIC-only listable, publishers sell-only; bid-time matching unchanged
103. ⬜ Data bartering - proposals, fairness scoring, mutual activation — deferred
104. ✅ Marketplace billing - CPM surcharge tracking, data provider payouts — **COMPLETE 2026-08-10**: CPM-surcharge settlement (mig 090, reporting-only; buyer debit / seller credit net / platform margin, ledger-balanced, exactly-once) on top of the earlier segtax data-fee payout spine (`data_fee_pending` durable join → ledger + owner balance, trusted-seat billing)

### ⬜ Phase 11: Business Operations  — not started
105. Account closure and data export workflow
106. adtech.js SDK versioning and CDN deployment pipeline
107. Public status page
108. Customer support / dispute resolution workflow
109. API changelog system
110. SSO (SAML 2.0 + OIDC) for enterprise accounts
111. Data residency controls
112. ✅ External partner onboarding portal (sandbox, test endpoint, certification) — COMPLETE 2026-08-12 (4 slices, each adversarially reviewed + fixed + e2e-green live). Slice 1 SHIPPED 2026-08-12: staff-facing partner registry (`partners` table mig 097, platform-global like incidents; `pkg/partner` state machine pending→sandbox→certified→active + off-ramps; gateway `/v1/api/partners` + `/v1/api/partners/status` on new `partners:read`/`partners:manage` perms; staff-portal "Partners" tab register/edit/transition; e2e `TestPartnerOnboardingLifecycle`). Slice 2a SHIPPED 2026-08-12: NEW `partner` account type (mig 098 + pkg/auth AccountPartner, perms partner:self/apikeys:manage); staff provision a partner login (POST /v1/api/partners/provision → creates the partner account+owner, links partners.account_id, returns a one-time temp password); partner self-serve portal (/portal/partner, partner.html — onboarding status via /v1/api/partner/me + integration guide); e2e TestPartnerProvisionAndSelfServe. Slice 2b SHIPPED 2026-08-12: partner self-serve sandbox API keys (mig 099 adds secrets purpose 'partner_sandbox'; gateway /v1/api/partner/sandbox-keys generate/rotate/list-masked + /revoke, account-scoped, apikeys:manage + partner-account-only; portal API-keys tab; mirrors the conversion-key rotation/grace pattern); e2e TestPartnerSandboxKeys. Slice 3 SHIPPED 2026-08-12: reusable OpenRTB conformance validator (pkg/openrtb/conformance.go — ValidateBidResponse/ValidateBidRequest + GoldenBidRequest, unit-tested: no-bid valid, price/floor/adm/impid/badv errors, crid/adomain warns) + partner self-serve tools (POST /v1/api/partner/validate = check a pasted BidResponse; POST /v1/api/partner/test-bid = live golden request to the partner's registered endpoint + validate, graceful on unreachable) + portal "Sandbox test" tab; e2e TestPartnerConformanceTools. Slice 4 SHIPPED 2026-08-12: certification suite — pkg/partner CertificationScenarios + ScoreCertification (golden scenarios, conformance-scored, unit-tested); mig 100 partner_certifications; gateway GET/POST /v1/api/partner/certify (partner runs the suite, a PASS while in sandbox auto-advances → certified; staff still gates certified→active); portal Certification tab; e2e TestPartnerCertification (conformant → certified; bad → stays sandbox). Hot-path integration SHIPPED 2026-08-12 (follow-up): the exchange merges 'active' DSP partners from the registry into the auction fan-out (exchange.partner_registry_enabled, default off; a background warm cache reloads them from Postgres so the auction reads an in-process snapshot — hot-path-iron-rule-safe; partner.ActiveDSPEndpoints formats endpoint;seat=;notify=; wired into the debug cache-refresh broadcast). e2e TestPartnerActiveInExchangeFanout (active partner in the fan-out only with the flag on). So an approved partner now receives LIVE bid traffic. Inbound auth SHIPPED 2026-09-10 (shared middleware.PartnerInboundAuth, sibling of AuthAPIKey): the exchange's EXTERNAL HTTP OpenRTB surface (/v1/openrtb/auction + Prebid, gated at the OUTERMOST layer so a rejected caller is 401'd BEFORE the Prebid handler reads the body / publishes identity) authenticates the caller against a per-partner sandbox key (X-API-Key, purpose=partner_sandbox, validated via the exchange's in-process secrets warm cache — hot-path-safe; revoked/expired keys rejected). On success the authenticated secret is exposed via SecretFromContext so a downstream handler CAN attribute to the trusted partner account (secret.AccountID) instead of a self-declared body field — the enforced effect TODAY is the 401; wiring attribution onto that account is a separate step. Gated by exchange.inbound_partner_auth_strict (TierLive, default false=warn/backwards-compat, true=401 on missing/invalid). The internal gRPC twin (our own SSP) is intentionally ungated. e2e TestPartnerInboundAuctionAuth. Still deferred: partner-STATUS re-check at request time (a terminated/paused partner whose key wasn't revoked still authenticates — offboarding must revoke keys), a distinct partner_prod credential for live vs sandbox traffic, win/loss-callback auth, gating exchange /readyz on the secrets cache (a cold-boot pod in strict mode can 401 valid keys until the first secrets load), and consuming the bound account in attribution.

### 🟨 Phase 12: CI/CD and Production Readiness  — partial (re-audited 2026-08-09); deploy pipelines (114–117) belong to the staging handoff (docs/handoffs/03-staging.md), not this phase's backlog
113. ✅ `ci.yml` - PR pipeline SHIPPED 2026-08-06 (c69136d): gofmt/vet gates in the test job + helm-validate job (helm lint + template | kubeconform-strict); no conditional A/B (deliberate)
114. ⬜ `nightly.yml` - full nightly (build, test, perf, chaos, security, summary) — → 03-staging
115. ⬜ `deploy-staging.yml` - auto-deploy on merge — → 03-staging
116. ⬜ `deploy-prod.yml` - manual with approval gate — → 03-staging
117. ⬜ `perf-test.yml` and `chaos-test.yml` - nightly regression — → 03-staging (local protocol exists: /perf-loadtest skill + make test-e2e-chaos)
118. ⬜ SOPS for secret management (referenced in values-staging.yaml; not wired)
119. ✅ HPA autoscaling per service — SHIPPED 2026-08-12. `templates/services.yaml` emits a `HorizontalPodAutoscaler` (autoscaling/v2, CPU utilisation) for any service with an `hpa:` block, gated by `global.autoscaling` (default on; needs metrics-server, ships with k3s). spec.replicas is omitted for hpa'd services so the controller owns the count. On the CPU-bound stateless serving fleet only (tracker min3/max8, adserver/gateway/ssp/exchange/dsp-internal max6, publisher-adserver/ssai max5 — all targetCPU 70); fixed-replica (non-autoscaled) services stay put — the multi-replica ones (reporting/webhooks/identity-consumer/report-runner) run 3 by config; the true singletons (pipeline/notifications/cronjobs) run 1. Verified live (all 8 HPAs read real CPU metrics). Paired with **PodDisruptionBudgets** (2026-08-12): the same template emits a PDB (maxUnavailable:1, `global.pdb` gated) for every service that is autoscaled OR runs >1 replica (so reporting/webhooks/identity-consumer/report-runner/audience-rt get one too) — never for a true singleton (a PDB on a 1-replica workload deadlocks its own drain) — so a node drain / rolling upgrade can't take a service to zero. NOTE: `/perf-loadtest` pins replicas (disable HPA) for comparable runs.
120. 🟨 Backup CronJobs — Postgres backup CronJob shipped (helm `backup-postgres.yaml`); WAL archiving + pre-migration snapshot open (DuckDB backup obsolete — ClickHouse is the analytical store, ADR 0006)
121. ✅ OpenAPI spec + Swagger UI (docs/openapi.yaml + gateway Swagger; reverse drift guard 2026-08-08)
122. ✅ D2 architecture diagrams (docs/diagrams/ + make diagrams + per-diagram "Update when" index)
123. ⬜ Final e2e testing with all simulation profiles + chaos scenarios

### Rule: every channel ships with its simulator template in the same PR.

---

## Build Status & Outstanding Work

> Single source of truth for **plan vs reality** (folded from the former
> `MOCK_AUDIT.md` + `OUTSTANDING_WORK.md`). Reconciled against disk 2026-07-01.

**Reality vs the phase claims.** Phases 1–8 above are marked done, and that is
true for the **synchronous serving spine** (SSP → Exchange → DSP → Ad Server →
Tracker → Reporting) plus persistence (analytics, rollups, datalake, billing
ledger) and security (secrets-at-rest, JWT). It **overstates the async / cron /
compliance edges**: nothing in an e2e ad-trace touches them, so several were
declared done on the strength of the hot path. Rule of thumb when picking up:
**anything scheduled, batch, or outward-facing is the least likely to be real.**

### Outstanding items (keyed by step #)

| Step | Item | State | Where / seam | Done when |
|---|---|---|---|---|
| 45 | Identity graph wired into serving | ✅ SHIPPED (closed by the profile-store + attribution epics, re-reconciled 2026-08-09) | `identity_graph` PG writes via `cmd/identity-consumer`; DSP read-time expansion behind `dsp.identity_resolution_enabled` (lazy resolver, no boot latch); batch clustering + person-level memberships via `cmd/profile-builder` + `identity_clusters` (`audiencepg.ExpandPerson` — used by durable purchase suppression, migration 082); SSP stamps public segments + household EIDs; cross-device view-through attribution resolves through the graph (confidence-floored). e2e: profile-store suites, `attribution_identity_bridge_test.go`, household suites — all green | ✅ done |
| 50 | `cmd/privacy-delete` + `cmd/privacy-verify` | ✅ SHIPPED (2026-07-05) | `pkg/privacydelete` (Deleter purges identity_graph + audience_segment_members for pending level-3 users, marks completed, announces `deletion_completed`; Verifier residual-checks + stamps verified_at) + both one-shot binaries + Tilt resources | ✅ `TestPrivacyDeletionPropagation` flipped |
| 58 | `cmd/fraud` batch CronJob | ⚠️ lib real, no binary; **blocked on data** | `pkg/fraud/{realtime,scoring,adstxt}`; blocklists already DB-driven. **NOTE (2026-07-06):** a velocity/IP sweep can't be built yet — the analytics `impressions`/`clicks` tables have **no IP column** (only geo/device), so there's nothing to aggregate suspicious IPs from. First add IP capture to the event schema, THEN the batch job scores + writes `fraud_blocklists` (tracker warm cache already consumes them). Detection logic lives in `pkg/fraud` (was under active app-ads.txt work — coordinate). | F-series batch-sweep assertion |
| 61 | `sellers.json` from DB | ✅ SHIPPED (2026-07-05) | `cmd/gateway/sellers.go` — `pgSellerStore` reads active `publishers` (seller_id = UUID); adding a publisher changes the output with no code change. DB-down → valid file with empty seller list, not stale hardcodes | ✅ done (handler unit tests: from-DB + empty-on-error) |
| 63 | `cmd/optimise` pipeline CronJob | ✅ CLOSED (re-reconciled 2026-08-09) — no standalone binary needed | Creative side live since before (adserver `warmStartBandit` seeds the bandit from per-creative CTR on boot). The one real gap — routing warm-start — shipped with the SmartRouter split-brain fix: `cmd/exchange/warmstart.go` boot-seeds `router.Seed` from global `dsp_calls`, `routingsync.go` does the periodic cross-replica reseed + idempotent reset broadcast. `cmd/optimise/` stays an empty dir like `cmd/rollup` (engine lives in-process; a standalone binary may never be needed) | ✅ done (live-stats + routing-knobs e2e green) |
| 76 | `cmd/webhooks` dispatcher | ✅ SHIPPED (2026-07-05) | `pkg/webhooks.Dispatcher` (store-backed, HMAC-signed envelope, retry+backoff, delivery log) + `cmd/webhooks` consuming `budget.depleted`/`balance.depleted`/`campaign.state_changed` from NATS → `webhooks`/`webhook_deliveries` tables; k8s pod + Tilt (port 8091). Remaining: more event types, delivery-log view API, DLQ on give-up | ✅ done (unit: httptest receiver verifies signed delivery + retry) |
| 77 | `pkg/email` real SMTP + Mailpit | ✅ SHIPPED (2026-07-05) | `SMTPSender.Send` builds RFC 5322 MIME + `smtp.SendMail` (auth optional via `NewSMTPAuth`); Mailpit deployment (`k8s/base/mailpit`, SMTP 1025 / UI 8025) wired into kustomize + Tilt; report-runner delivers via `REPORT_RUNNER_SMTP_HOST=127.0.0.1:1025`. Remaining: e2e assertion reading a message out of Mailpit's API | ✅ done (unit: throwaway SMTP server captures DATA end-to-end) |
| 72 | Chaos framework (`harness.ChaosKill*`) | ✅ SHIPPED (2026-07-05) | `tests/e2e/harness/chaos.go` — `WithChaos`/`ChaosKill{Redis,NATS,Postgres,Minio}`/`ChaosWaitReady` wrap `kubectl -n adtech delete pod` + recovery wait; the 4 `chaos_test.go` cases now assert graceful degradation (self-skip w/o kubectl) | ✅ 4 chaos e2e cases flipped |
| 70–71 | `pkg/simulator` / `tests/k6` | 🟨 sim substantial, k6 superseded locally (re-reconciled 2026-08-09) | `pkg/simulator` grew real with the production-like epic (persona builder, themed personas/user pools, `pages` = multi-slot demo pages, all formats incl. SSAI/CTV); `tests/k6` remains 2 scripts — the actual perf harness is `make loadtest` + the /perf-loadtest protocol (phase metrics, VERIFY money invariant, canary). k6 scripts only matter again for CI perf regression (→ 03-staging, step 117) | perf regression wired into nightly CI |
| — | Event-spool residual: pod EVICTION loses undrained events | ⚠️ KNOWN LIMITATION (2026-08-05, deliberate deferral) | `pkg/events/spool.go` — the disk spool (shipped e9256bf, chaos-proven by `TestChaosNATSOutageSpoolLossless`) writes to an **emptyDir**, which survives container restarts (probe-kill storms — the observed failure mode) but NOT pod eviction/deletion/node loss. If a pod is evicted while holding undrained events (i.e. during a NATS outage), those events are gone. Window = (NATS down) ∩ (pod evicted) — both were true simultaneously exactly once (the 2026-08-05 VM seizure). Two real fixes, both with costs: (a) PVC-backed spool — per-pod RWO volumes complicate scheduling/rollouts for stateless services; (b) transactional outbox — bulletproof but puts a synchronous Postgres write on the auction/tracker hot path (~1-5ms + a new hard dependency). Revisit when: prod runs on preemptible/spot nodes, eviction becomes routine (HPA churn), or a money audit shows losses matching eviction events. Until then the exposure is monitored: `adtech_events_spool_bytes` > 0 during any eviction = at-risk events; `adtech_events_dropped_total` alerts on actual cap-drops. | either (a)/(b) shipped + chaos e2e extended to `kubectl drain`-style eviction with spool non-empty |
| — | Ops console + `devops` role (in-cluster) and host dev console (Tilt-UI replacement) | ✅ SHIPPED (2026-07-19) — staff portal Ops section (pkg/kubeops hand-rolled k8s REST client, /v1/api/ops/* gated ops:read/ops:deploy, audited actions, gateway ServiceAccount RBAC, devops@adtech.local seed login, migration 046) + cmd/devconsole (`make devconsole`, localhost:8099) | Part 1: staff portal section gated by a new `devops` role — pod matrix / rollout-restart / log tail / manual CronJob triggers / NATS lag / PVC usage / readyz grid, via a namespace-scoped ServiceAccount from the gateway; same console serves as the prod monitoring+actions surface (Grafana/Jaeger remain for metrics/traces — this is for ACTING on the stack). House rules apply: toast+undo not confirm dialogs, per-pod granularity, audit every action. Part 2: `cmd/devconsole` host binary (`make devconsole`) for build/deploy-per-service buttons + build output stream — host-only because builds need the Go toolchain + docker socket; deliberately thin (a face on `make deploy SVC=x`), not a Tilt rebuild. | devops-role user can restart a pod + trigger the conductor from the portal (audited); local `make devconsole` builds+deploys a service from the browser |
| — | Dev-orchestration migration: Tilt+OrbStack → Helm charts on Rancher Desktop (k3s) | ✅ SHIPPED 2026-07-18 | Stack runs on Rancher Desktop k3s via `k8s/helm/adtech` (per-env values); dev loop = `make stack-up` / `make deploy SVC=x`; Tiltfile DELETED; e2e suite green on it. See k8s/CLAUDE.md | ✅ done |

**Sub-items still open on things marked ✅** (not blockers): analytics `/debug`
read-backs are memory-only; rollup query API doesn't read by tier;
`cmd/pipeline` datalake worker + compaction; ClickHouse flip to local default +
batch inserts; bind audience upload to JWT not body; two-key JWT rotation +
seed a dev signing key.

**Recently closed** (2026-07-01): TigerBeetle OOM crashloop (4Gi limit);
gateway→exchange traceparent propagation (one trace_id end-to-end); rollup
re-runs now idempotent (replace-by-window on all three backends); tracker reads
`X-Forwarded-For` (IP-block e2e flipped); trace-explorer batch-reconciliation view.

### Do NOT "fix" these — deliberate, not defects

- 🟢 **Resilience fallbacks** (real path runs by default; stand-in triggers only
  when infra is down): Redis L2 → `MemoryL2`; object store `s3` → `fs`; NATS →
  HTTP bridge to reporting; Postgres warm caches → YAML / empty; config poll →
  env → code defaults. *Prod caveat:* these are **fail-open** (freq-cap / dedup /
  pacing mis-count if Redis is down) — for prod, make Redis a readiness
  requirement so a cache-less pod drops out of rotation.
- 🟡 **Intentional simulation:** competitor DSP `noise_pct` / `no_bid_rate`
  (`cmd/dsp/main.go`) is the demo market generator.
- `cmd/rollup` is empty but the rollup engine runs inside `cmd/reporting`, and
  `cmd/optimise` is empty but both optimisation seams run in-process (bandit in
  adserver, routing warm-start in exchange) — standalone binaries may never be
  needed. `cmd/cleanroom` is empty because Phase 10 is correctly not started;
  `cmd/fraud` is empty pending step 58 (blocked on IP capture in the event
  schema). (`cmd/{ssai,transcoder}` HAVE since been built — Phase 9 core.)

### Test-harness debt (skips are missing helpers, not missing features)

`harness.ChaosKill{NATS,Redis,Postgres,Minio}` ✅ SHIPPED 2026-07-05
(`tests/e2e/harness/chaos.go`; the 4 chaos tests now assert graceful
degradation). Contract-write helper `harness.SetPublisherContract` ✅ SHIPPED
2026-07-06 (writes `publishers.revshare_config` + invalidates billing-rates);
`TestBillingGuaranteedMinimumSubsidy` flipped. Still-open harness gaps: deal-type
modifier needs deal_type to propagate impression→SpendEvent; tiered-RS needs the
settle path to populate `Contract.MonthImpressions` + a `FireNAuctions` tier
cross; reservation-expiry needs a cron helper + low-TTL knob; multi-currency
needs an `exchange_rates` seed + non-USD campaign; observability/migration need a
Jaeger client wrapper + single-step migration mode.

### Suggested pickup order

1. ~~`harness.ChaosKill*` helpers (72)~~ ✅ done 2026-07-05 (4 chaos e2e cases flipped; billing-helper gaps remain).
2. ~~SMTP + Mailpit (77)~~ ✅ done 2026-07-05 (scheduled reports now actually deliver).
3. ~~`sellers.json` from DB (61)~~ ✅ done 2026-07-05 (served live from the publishers table).
4. ~~`cmd/webhooks` (76)~~ ✅ done 2026-07-05 (dispatcher delivers signed events with retries).
5. ~~`cmd/privacy-delete` / `-verify` (50)~~ ✅ done 2026-07-05 (level-3 deletion pipeline + verifier; e2e flipped).
6. Contract-write helper (`harness.SetPublisherContract`) ✅ done 2026-07-06; `TestBillingGuaranteedMinimumSubsidy` flipped.

**Next up (2026-07-06), in confidence order:**
7. **Finish the billing-model tests** — collision-free, flips real skips. (a) deal-type modifier: verify `deal_type` propagates impression→`SpendEvent`, then flip `TestBillingDealTypeFeeModifier`; (b) tiered-RS: populate `Contract.MonthImpressions` on the settle path + `FireNAuctions` to cross a tier; (c) multi-currency: seed `exchange_rates` + a non-USD campaign. Helper already exists for (a)/(b).
8. `cmd/optimise` routing warm-start (63) — small once `cmd/exchange` is free (see ledger note); creative side already done.
9. `cmd/fraud` batch (58) — **needs IP added to the event schema first** (see ledger note), then wraps `pkg/fraud`.
10. Identity-graph wiring (45) — lowest urgency, nothing depends on it.

> Coordination: 58 and 63 both touch files (`pkg/fraud`, `cmd/exchange`) that were under concurrent edit on 2026-07-06 — sequence them after that work merges to avoid conflicts.

### Ideas — could do, not scheduled

- **Text-video channel (ASCII/cell-grid streaming) — clean-room, our own Go implementation.**
  Inspired by [ASCILINE](https://github.com/YusufB5/ASCILINE) (researched 2026-07-10): a
  server renders each video frame to a grid of colored text cells and pushes
  delta-encoded binary frames over a WebSocket; the browser paints them onto a
  Canvas. No `<video>` element and no separate ad request, so ad frames are
  indistinguishable from content frames — SSAI taken to its logical extreme
  (the server owns the frame pipeline; ad insertion = switching the frame
  source at a cue point). Tracking becomes fully server-authoritative and
  frame-accurate: the server knows exactly which frames it delivered, so
  impressions/quartiles fire on frame delivery — stronger than the current
  segment-fetch beacons and a good "zero data slippage" showcase.
  **License constraint: we CANNOT use ASCILINE's code** — its MIT license has an
  explicit anti-advertisement clause (ad-serving use terminates the license).
  The technique isn't restricted, so we build our own: a small Go frame
  streamer (ffmpeg decode via the `pkg/transcode` seam, pixel→cell mapping,
  RAW/zlib/delta frame codec, WebSocket push), reusing `cmd/ssai`'s SSP
  auction + beacon plumbing at ad breaks, plus a vanilla-JS Canvas player page
  in `web/`. Scope if picked up: one demo service (or a `cmd/ssai` WebSocket
  endpoint), player page, e2e test. Known ceilings (fine for a demo channel):
  ~360p max, per-viewer server render cost (no CDN segment caching), no DRM.
  **Decision (2026-07-10): ASCILINE may run locally as an ad-free reference
  testbed only** (`make asciline-demo` / Tilt `asciline-demo` — clones into
  gitignored `third_party/`, streams a generated test clip). It must never be
  wired to the SSAI stitcher, SSP, or any ad path — even simulated ads would
  violate its anti-advertisement license clause. The production-shaped idea
  above remains a from-scratch Go implementation.

---

## Business Operations (Running the Platform)

These are "running the business" concerns beyond the ad tech features. Essential for operating commercially.

### 1. Account Closure and Data Export

When an advertiser or publisher leaves the platform:

```
Account owner requests closure
    |
    v
Grace period starts (30 days):
    - Campaigns paused, no new spend
    - Publisher placements deactivated, no new auctions
    - Data export available for download
    |
    v
Final settlement:
    - Advertiser: final invoice generated, outstanding balance collected or refunded
    - Publisher: final payout calculated, remaining earnings paid out
    - Adjustments: any pending disputes resolved
    |
    v
Data export package generated:
    - Campaign performance history (CSV/JSON)
    - Creative assets (zip download from S3)
    - Audience lists (hashed, exportable)
    - Invoice/payout history (PDF)
    - Audit log for their account (CSV)
    |
    v
After grace period:
    - Account marked as closed
    - Data retained for 90 days (legal/audit requirement)
    - After 90 days: data purged (same deletion pipeline as GDPR)
```

**API:**
- `POST /v1/api/account/close` - initiate closure (starts grace period)
- `GET  /v1/api/account/export` - download data export package
- `POST /v1/api/account/close/cancel` - cancel closure during grace period

### 2. adtech.js SDK Versioning

Publishers embed our JavaScript tag in their pages. We don't control when they update. SDK versioning must be backwards-compatible and gracefully managed.

**Versioned CDN URLs:**

```
https://cdn.adtech.example.com/sdk/v1/adtech.js       # major version 1 (stable)
https://cdn.adtech.example.com/sdk/v1.2/adtech.js     # minor version (opt-in)
https://cdn.adtech.example.com/sdk/latest/adtech.js    # always latest stable (not recommended for prod)
```

**Version lifecycle:**

| Phase | Duration | What happens |
|---|---|---|
| Active | Indefinite | Receives bug fixes and new features |
| Deprecated | 6 months | Still works but console warning: "v1 is deprecated, upgrade to v2" |
| Sunset | 3 months after deprecation end | Returns a fallback ad with upgrade notice |
| Removed | After sunset | Returns empty response, publisher must upgrade |

**Breaking change policy:**
- Minor versions (v1.1 -> v1.2): backwards compatible, publisher can stay on v1
- Major versions (v1 -> v2): may have breaking changes, 6-month migration window
- Hotfixes: pushed to all active versions simultaneously

**SDK hosted on CDN** with long cache TTLs (1 hour) and versioned paths so cache-busting happens on version bump, not on every deploy.

### 3. Public Status Page

External-facing page showing platform health for customers.

**Components displayed:**

| Component | What customers see |
|---|---|
| Ad Serving | "Operational" / "Degraded" / "Outage" |
| Tracker | "Operational" / "Degraded" / "Outage" |
| Dashboard & API | "Operational" / "Degraded" / "Outage" |
| Reporting | "Operational" / "Delayed" |
| Billing | "Operational" / "Degraded" |
| Data Pipeline | "Operational" / "Delayed" |

**Features:**
- Automated status from Prometheus alerts (P1/P2 -> component shows degraded/outage)
- Incident timeline with updates ("Investigating" -> "Identified" -> "Monitoring" -> "Resolved")
- Planned maintenance windows (announced 72h in advance)
- Historical uptime percentage (99.9% target)
- RSS/email subscription for status updates

**Implementation:** Self-hosted (Go service, minimal dependencies) or third-party (Statuspage.io, Instatus). Served at `status.adtech.example.com`. Updated automatically from Grafana alert state via webhook.

### 4. Customer Support and Dispute Resolution

**Support channels:**

| Channel | For | Response SLA |
|---|---|---|
| In-dashboard support widget | General questions, account help | 24h |
| Email (support@) | Billing disputes, technical issues | 24h |
| Priority support (enterprise) | Dedicated account manager | 4h |

**Dispute resolution workflow:**

```
Advertiser raises dispute:
    "Your tracker shows 80K impressions but my DoubleVerify shows 50K"
    |
    v
POST /v1/api/support/disputes
    {type: "impression_discrepancy", amount_disputed: 30000, evidence: "DV report attached"}
    |
    v
Support team receives ticket:
    1. Pull platform impression data for the date range + campaigns
    2. Pull third-party pixel fire rates (if our pixels + their pixels, compare counts)
    3. Pull fraud scores for the disputed impressions
    4. Identify discrepancy cause:
        a. Viewability difference (we count rendered, they count viewable)
        b. Geography difference (different geo databases)
        c. Bot filtering difference (different bot lists)
        d. Genuine tracking discrepancy (data loss)
    |
    v
Resolution:
    a. Discrepancy explained -> no adjustment, explanation sent
    b. Platform error confirmed -> billing adjustment issued (credit)
    c. Partial discrepancy -> negotiate settlement
    |
    v
All disputes tracked in Postgres with audit trail
Dashboard: /v1/api/support/disputes (list, detail, resolve)
```

### 5. API Changelog and Communication

**Changelog format (in repo):**

```markdown
# docs/CHANGELOG.md

## v1.12.0 (2026-06-15)
### Added
- `GET /v1/api/audiences/{id}/analytics` - audience composition analytics
- Video ad format support (VAST 4.2)

### Changed
- `POST /v1/api/campaigns` now requires `insertion_order_id` field

### Deprecated
- `GET /v1/api/reports/campaigns` - use custom report builder instead. Removal: v1.15.0

### Fixed
- Frequency cap race condition under high concurrency
```

**Communication channels:**
- Changelog page in dashboard (auto-generated from `docs/CHANGELOG.md`)
- Email notification to API key owners when breaking changes are announced
- Webhook event `adtech.webhooks.api.changelog` for automated integrations
- Deprecation warnings in API response headers: `Sunset: Sat, 01 Nov 2026 00:00:00 GMT`

### 6. SSO / Enterprise Authentication

Enterprise customers require SSO with their identity provider.

**Supported protocols:**

| Protocol | Use case |
|---|---|
| SAML 2.0 | Enterprise IdPs (Okta, Azure AD, OneLogin) |
| OIDC (OpenID Connect) | Modern IdPs, Google Workspace |
| OAuth2 | Third-party app integrations |

**How it works:**

```
Enterprise user visits dashboard
    |
    v
Gateway checks: does this account have SSO configured?
    +-- No  -> show login form (email/password + JWT, existing flow)
    +-- Yes -> redirect to IdP (Okta, Azure AD)
    |
    v
User authenticates with IdP
    |
    v
IdP redirects back with SAML assertion / OIDC token
    |
    v
Gateway validates assertion, maps IdP user to platform account
    |
    v
Issues platform JWT (same as normal login from here)
```

**Configuration per account:**
```json
{
    "account_id": "adv_enterprise_123",
    "sso": {
        "enabled": true,
        "protocol": "saml",
        "idp_entity_id": "https://okta.enterprise.com/app/xxx",
        "idp_sso_url": "https://okta.enterprise.com/app/xxx/sso/saml",
        "idp_certificate": "-----BEGIN CERTIFICATE-----...",
        "attribute_mapping": {
            "email": "user.email",
            "name": "user.displayName",
            "role": "user.groups"
        },
        "auto_provision_users": true,
        "default_role": "viewer"
    }
}
```

**API:**
- `GET  /v1/api/account/sso` - get SSO configuration
- `PUT  /v1/api/account/sso` - configure SSO
- `POST /v1/api/account/sso/test` - test SSO connection

Implementation in `pkg/auth/sso/` - SAML and OIDC handlers. Gateway middleware checks SSO before showing login form.

### 7. Double-Entry Accounting Ledger

Every money movement is recorded as a debit AND a credit. The books always balance.

**Ledger accounts:**

| Account | Type | What it tracks |
|---|---|---|
| `advertiser:{id}:balance` | Asset | What the advertiser owes us (or prepaid credit) |
| `publisher:{id}:earnings` | Liability | What we owe the publisher |
| `platform:revenue` | Revenue | Platform's earned margin |
| `platform:cash` | Asset | Cash received from advertisers |
| `advertiser:{id}:credit` | Liability | Advertiser's prepaid balance (we owe them service) |

**Every financial event creates two entries:**

```
Impression served (CPM billing):
    Debit:  advertiser:adv_123:balance   $0.003  (advertiser owes more)
    Credit: publisher:pub_456:earnings    $0.0024 (we owe publisher more)
    Credit: platform:revenue              $0.0006 (our margin)

Advertiser tops up balance:
    Debit:  platform:cash                 $1000   (we received cash)
    Credit: advertiser:adv_123:credit     $1000   (we owe them service)

Publisher payout:
    Debit:  publisher:pub_456:earnings    $500    (reduce what we owe)
    Credit: platform:cash                 $500    (cash leaves our account)

Fraud adjustment (credit back to advertiser):
    Debit:  platform:revenue              $10     (reduce our revenue)
    Credit: advertiser:adv_123:balance    $10     (reduce what they owe)
```

**Invariant:** `SUM(debits) == SUM(credits)` always. If they don't balance, there's a bug. Checked by a daily verification job.

**Financial reports for auditors:**
- Trial balance (all accounts with balances)
- Accounts receivable aging (which advertisers owe money, for how long)
- Accounts payable (what we owe publishers)
- Revenue recognition (when revenue was earned)
- Cash flow (money in vs money out)

Implemented in `pkg/billing/ledger.go`. Every billing operation writes ledger entries atomically alongside the spend/payout records.

### 8. Data Residency and Cross-Border Controls

EU data stays in EU infrastructure. Configurable per region.

**How it works:**

| Configuration level | What it controls |
|---|---|
| Platform-wide | Default data region (e.g. `eu-west-1`) |
| Per publisher | Publisher's user data stored in their region |
| Per advertiser | Advertiser's CRM data stored in their region |

**Multi-region deployment (when needed):**

```
EU Region (eu-west-1):
    k3s cluster with: all services
    Postgres: EU user data, EU publisher data
    Analytics: EU events
    S3: EU creative assets

US Region (us-east-1):
    k3s cluster with: all services
    Postgres: US user data, US publisher data
    Analytics: US events
    S3: US creative assets

Cross-region:
    NATS: federated (events route to correct region)
    Exchange: can bid on inventory in any region
    Reporting: queries route to correct region's analytics store
```

**For MVP:** single region deployment. Data residency is a configuration flag that restricts where user data is stored. Full multi-region is a future phase.

**BUILT (2026-09-10):** MVP data residency shipped as two slices.
*Control plane* — `accounts.residency_region` (mig 103) + a `platform.region`
config key (this deployment's home region, default `us-east-1`); the region rides
the JWT (`auth.Claims.ResidencyRegion`, loaded at login) and `middleware.Auth`'s
region gate rejects out-of-region accounts' MUTATIONS with 403 (reads allowed,
staff/admin exempt). Staff pin an account via `PUT /v1/api/accounts/residency`
(`support:update`, audited). *Data plane* — the SSP reads `regs.ext.data_residency`
and, when it differs from `platform.region`, suppresses all user-level data
emission (identity-graph observe, behaviour observe, and the audience
segment/taxonomy stamp onto the outbound bid request) via
`privacy.AllowsUserData`; the flag propagates downstream on `regs.ext`. Empty
region on either side disables the gate, so single-region deployments are
unaffected.

*Scope of the MVP enforcement (be precise — this is a compliance surface):* the
control-plane gate covers **gateway HTTP JWT-authed mutations only**; the SSP
data-plane gate covers the **bid/serve path** (segments, identity + behaviour
observe, and — because the beacon user key is the gated `behaviourUserKey` — the
tracker beacon that path emits). **NOT yet gated** (documented follow-ups, not
holes we claim to close): API-key ingest paths (e.g. `POST /v1/api/identity-links`)
and non-serve tracker entries (retargeting pixel, conversion postback), which
carry their own user id and never see `data_residency`; internal gRPC and
NATS-consumer/background writes; **act-as** (the gate evaluates the *caller's*
region, so an in-region agency acting-as an out-of-region advertiser is not
blocked); the residency signal is **self-declared** on the request and permissive
by omission (not yet bound to the publisher account's region); the region rides a
**12h JWT** (a staff region change applies at next login, no revocation tie-in);
and an out-of-region account exercises data-subject rights (export/close) in its
**home** region. Opt-out writes are deliberately never residency-blocked. Also not
built: actual multi-region infra (federated NATS, per-region stores/buckets,
region-routed reporting).

**Data residency flag in bid request:**

```json
{
    "regs": {
        "ext": {
            "data_residency": "eu"
        }
    }
}
```

Services check this flag before storing user-level data. If `data_residency: eu`, user data only written to EU Postgres/analytics/Redis instances.

### Implementation

| Component | Location |
|---|---|
| Account closure | `pkg/account/closure.go` - grace period, final settlement, data export |
| Data export | `pkg/account/export.go` - generates downloadable package |
| SDK versioning | CDN deployment pipeline + `web/static/` versioned builds |
| Status page | `cmd/statuspage/` or third-party integration via Grafana webhook |
| Support/disputes | `pkg/support/` - ticket CRUD, dispute workflow |
| API changelog | `docs/CHANGELOG.md` + Gateway serves at `/changelog` |
| SSO | `pkg/auth/sso/` - SAML 2.0 + OIDC handlers |
| Double-entry ledger | `pkg/billing/ledger.go` - debit/credit entries, trial balance |
| Data residency | `pkg/privacy/residency.go` - region routing, storage restrictions |

---

## Production Resilience and Developer Experience

Critical architectural decisions that must be established in Phase 1 to avoid costly retrofits.

### 1. Redis Budget Recovery via NATS Replay

Redis holds budget balances for fast access, but NATS JetStream is the true financial ledger. If Redis loses data, we replay from NATS.

```
Normal operation:
    AuctionWinEvent in NATS (persisted, 7-day retention) <- TRUE LEDGER
        |
        v
    Redis DECRBY (fast balance cache)
        |
        v
    Periodic flush to Postgres (durable snapshot, every 10s)

Redis failure and recovery:
    1. Redis comes back empty
    2. Load last known balance from Postgres (snapshot from last flush)
    3. Query NATS: replay all AuctionWinEvents since last flush timestamp
    4. Apply all DECRBY operations from replay
    5. Redis balance now matches reality

    Max overspend window = 0 (every event is in NATS)
    Max data loss = 0 (replay reconstructs exact state)
```

**Recovery function in `pkg/cache/redis/recovery.go`:**

```go
func (r *BudgetCache) RecoverFromNATS(ctx context.Context) error {
    // 1. Load last snapshot from Postgres
    snapshots := r.postgres.GetLatestBudgetSnapshots()
    for _, snap := range snapshots {
        r.redis.Set("dsp:budget:"+snap.CampaignID, snap.Balance)
    }

    // 2. Replay events since snapshot
    lastFlush := r.postgres.GetLastFlushTimestamp()
    events := r.nats.Replay("adtech.auction.win", since: lastFlush)
    for event := range events {
        r.redis.DecrBy("dsp:budget:"+event.CampaignID, event.ClearingPrice)
    }

    // 3. Verify: Redis total == Postgres total + replayed decrements
    return r.verify()
}
```

Triggered automatically on DSP startup if Redis returns empty for known campaigns. Also runnable manually via Tilt button or ops API.

### 2. Postgres Connection Pooling (PgBouncer)

PgBouncer sits between all services and Postgres, multiplexing hundreds of app connections into a small pool of Postgres connections.

```
Services (500+ connections at scale)
    |
    v
PgBouncer (transaction-mode pooling)
    |
    Multiplexes into 50 Postgres connections
    |
    v
Postgres primary (max_connections = 100)
```

**Read/write split:**

| Traffic type | Route to | Services |
|---|---|---|
| Writes | PgBouncer -> Postgres primary | DSP (budget), Gateway (auth), Reporting (billing accrual) |
| Reads | PgBouncer -> Postgres standby (read replica) | Reporting (queries), Pipeline, all CronJobs, fraud batch, optimise, seed |

Configuration:
- `POSTGRES_URL` (writes) -> `pgbouncer-primary.default.svc:5432`
- `POSTGRES_READ_URL` (reads) -> `pgbouncer-standby.default.svc:5432`
- Services that only read use `POSTGRES_READ_URL` automatically via `pkg/store/postgres/` config

Added to K8s: `k8s/base/pgbouncer/` deployment with primary and standby pools.

### 3. DuckDB to ClickHouse Migration Path

**Dual-write from day one in staging.** When the time comes to switch, it's a config change, not a migration.

```
Reporting service processes event:
    |
    v
    analytics_backend config = "duckdb" (default)
    |
    +-> Write to DuckDB (primary - used for queries)
    +-> Write to ClickHouse (shadow - not queried, just writes) [staging/prod only]

Weekly verification CronJob:
    Run 10 standard queries against both stores
    Compare results (row counts, sum of spend, etc.)
    If match rate < 99.9% -> alert

Migration trigger (any of):
    - NATS consumer lag > 30s sustained for 5 minutes
    - DuckDB query p99 > 5 seconds
    - Reporting pod OOM killed

Migration (one config change):
    analytics_backend = "clickhouse"
    Restart reporting service
    All queries now go to ClickHouse
    DuckDB continues receiving writes for 24h (rollback safety)
    After 24h: disable DuckDB writes

Rollback:
    analytics_backend = "duckdb"
    Restart reporting service
    (Both stores have the same data from dual-write)
```

The `pkg/store/analytics/` interface means queries work identically against either backend. The dual-write is just writing to both implementations.

### 4. Idempotent Event Processing

Every NATS consumer must handle duplicate delivery. Standard pattern in `pkg/events/consumer.go`:

```go
// pkg/events/consumer.go
type IdempotentConsumer struct {
    redis  *redis.Client
    inner  EventHandler
    ttl    time.Duration  // dedup key TTL (default 24h)
}

func (c *IdempotentConsumer) Handle(ctx context.Context, event *Event) error {
    // Dedup key: trace_id + event_type
    dedupKey := fmt.Sprintf("dedup:%s:%s", event.TraceID, event.Type)

    // Atomic check-and-set: returns true if key was NEW (not duplicate)
    isNew, err := c.redis.SetNX(ctx, dedupKey, "1", c.ttl).Result()
    if err != nil {
        return err // Redis error - NAK, retry later
    }
    if !isNew {
        return nil // Duplicate - already processed, ACK silently
    }

    // Process the event
    err = c.inner.Handle(ctx, event)
    if err != nil {
        // Processing failed - remove dedup key so retry works
        c.redis.Del(ctx, dedupKey)
        return err // NAK - NATS will redeliver
    }

    return nil // ACK
}
```

**Every consumer wraps with this automatically:**

```go
// In any service that consumes NATS:
consumer := events.NewIdempotentConsumer(redis, myHandler, 24*time.Hour)
eventBus.Subscribe(ctx, "adtech.auction.win", consumer)
```

**Dedup keys:**
- Stored in Redis with 24h TTL (NATS won't redeliver after max_deliver attempts, well within 24h)
- Key format: `dedup:{trace_id}:{event_type}` - ~100 bytes per key
- At 5000 events/sec: ~432M keys/day, ~43GB Redis. For the dedup Redis instance, this is fine with TTL cleanup.
- Could use a Bloom filter for even more memory efficiency if needed.

**This is mandatory for all consumers.** The `pkg/events/` Subscribe function wraps with idempotent consumer by default. Opting out requires explicit `SubscribeRaw()`.

### 5. Fast Inner Dev Loop (go run, not Docker)

Docker rebuilds are slow (~30s). For the inner dev loop, use `go run` directly with Tilt's `local_resource`. Container builds only for testing K8s-specific behaviour.

**Two Tilt modes:**

```python
# Tiltfile

dev_mode = os.getenv('DEV_MODE', 'fast')  # 'fast' or 'container'

if dev_mode == 'fast':
    # Inner dev loop: go run directly, ~2s rebuild
    local_resource('dsp', serve_cmd='go run ./cmd/dsp', deps=['cmd/dsp', 'pkg/'])
    local_resource('exchange', serve_cmd='go run ./cmd/exchange --channel=all', deps=['cmd/exchange', 'pkg/'])
    local_resource('tracker', serve_cmd='go run ./cmd/tracker', deps=['cmd/tracker', 'pkg/'])
    # ... etc for each service

    # Infrastructure still runs in K8s (Postgres, NATS, Redis, Minio)
    k8s_yaml(kustomize('k8s/overlays/local'))

else:  # 'container'
    # Full container builds, same as CI/staging/prod
    # ... existing Docker build logic
```

**Default: `tilt up` uses fast mode.** Services run as `go run` processes on the host, connecting to infra in K8s. Changes rebuild in ~2 seconds (Go compile) instead of ~30 seconds (Docker build + push + deploy).

**Switch to container mode** when testing Dockerfiles, K8s manifests, or deployment behaviour: `DEV_MODE=container tilt up`.

**Also offer a lite profile** for constrained machines:

```python
# PROFILE=lite tilt up -> skip observability stack
profile = os.getenv('PROFILE', 'full')

if profile == 'lite':
    k8s_yaml(kustomize('k8s/overlays/local-lite'))  # no Prometheus, Grafana, Loki, Jaeger
else:
    k8s_yaml(kustomize('k8s/overlays/local'))       # full stack
```

### 6. External Partner Onboarding

Self-service portal for DSPs and SSPs wanting to integrate.

**Partner integration flow:**

```
1. Partner visits /partners -> registers for sandbox access
    -> Gets: API key, sandbox endpoint URL, integration guide link

2. Partner reads integration guide (/docs/partners/):
    - DSP guide: how to respond to bid requests, VAST for video, timeout requirements
    - SSP guide: how to send bid requests, user signal format, floor price mechanics

3. Partner tests against sandbox:
    POST /v1/openrtb/test
    Body: partner's test bid response
    Response: {
        valid: true,
        warnings: ["bid.adomain missing - recommended for quality filtering"],
        latency_ms: 45,
        format_score: "92/100"
    }

4. Partner runs certification suite:
    POST /v1/openrtb/certify?partner_id=xxx
    -> Platform sends 100 test bid requests (display, native, video, various geos/devices)
    -> Validates: response format, timeout compliance (<100ms), floor respect, VAST validity
    -> Response: {
        passed: 87,
        failed: 13,
        details: [
            {test: "video_vast_response", result: "fail", reason: "missing companion ad"},
            {test: "timeout_compliance", result: "pass", latency_p99: "82ms"}
        ],
        certification: "conditional - fix 13 failures and rerun"
    }

5. Once certified: partner promoted to production
    -> Starts with 1% traffic share, monitored for 24h
    -> If healthy: ramp to 10%, then 50%, then full
    -> Automated ramp-down if error rate exceeds threshold
```

**Sandbox:** staging cluster with `demo` seed profile. Partners hit `sandbox.exchange.adtech.example.com`. Real auctions, fake money.

**Health check endpoint for partners:**
```
GET /v1/openrtb/health?partner_id=xxx
Response: {
    status: "healthy",
    last_bid_request_sent: "2026-05-28T10:30:00Z",
    last_response_received: "2026-05-28T10:30:00Z",
    avg_response_time_ms: 45,
    timeout_rate_24h: "2.1%",
    win_rate_24h: "12.5%",
    error_rate_24h: "0.1%"
}
```

### 7. Clock Abstraction

Foundational. Must be in Phase 1 before any time-dependent code is written.

```go
// pkg/clock/clock.go
type Clock interface {
    Now() time.Time
    Since(t time.Time) time.Duration
    Until(t time.Time) time.Duration
    After(d time.Duration) <-chan time.Time
    NewTicker(d time.Duration) *Ticker
}

// Real clock (production)
type Real struct{}
func (Real) Now() time.Time                        { return time.Now() }
func (Real) Since(t time.Time) time.Duration       { return time.Since(t) }
func (Real) Until(t time.Time) time.Duration       { return time.Until(t) }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (Real) NewTicker(d time.Duration) *Ticker     { return time.NewTicker(d) }

// Fake clock (tests)
type Fake struct {
    mu      sync.Mutex
    current time.Time
}
func (f *Fake) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.current }
func (f *Fake) Advance(d time.Duration) { f.mu.Lock(); f.current = f.current.Add(d); f.mu.Unlock() }
func (f *Fake) Set(t time.Time) { f.mu.Lock(); f.current = t; f.mu.Unlock() }
```

**Rule: no `time.Now()` calls in application code.** All services receive a `clock.Clock` at startup:

```go
// cmd/dsp/main.go
func main() {
    clk := clock.Real{}  // production
    pacer := pacing.New(clk, ...)
    server := dsp.New(clk, pacer, ...)
}
```

**What uses the clock:**

| Feature | Clock usage |
|---|---|
| Budget pacing | "At 3pm, should have spent 62.5% of daily budget" |
| Dayparting | "Is it between 9am-5pm in advertiser's timezone?" |
| Attribution windows | "Was this impression within 7 days of the conversion?" |
| Frequency cap TTLs | "Has 24 hours passed since this cap was set?" |
| CPC/CPA reservation expiry | "Has the attribution window closed? Release the hold." |
| Data retention | "Is this raw event older than 48 hours? Purge." |
| Session timeout | "Has this SSAI session been inactive for 30 minutes?" |
| Rate limiting windows | "How many requests in the last 60 seconds?" |
| Auction timeout | "Has 100ms elapsed since we sent bid requests?" |
| Creative review | "Has this creative been pending review for > 48 hours? Alert." |

**Test example:**

```go
func TestAttributionWindow(t *testing.T) {
    clk := &clock.Fake{current: time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)}
    attr := attribution.New(clk, windowDays: 7)

    // Impression at noon today
    imp := attr.RecordImpression("trace_1", "campaign_1")

    // Conversion 3 days later - within window
    clk.Advance(3 * 24 * time.Hour)
    result := attr.Attribute("trace_2", "campaign_1", "purchase")
    assert(result.Type == "view_through")
    assert(result.LagHours == 72)

    // Conversion 8 days later - outside window
    clk.Advance(5 * 24 * time.Hour)
    result = attr.Attribute("trace_3", "campaign_1", "purchase")
    assert(result == nil) // no attribution, window expired
}
```

### 8. Analytics Schema Evolution Strategy

**Rule: new columns always get `NULL` default. Forward-only by default. Backfill only when necessary.**

**Adding a new column to event tables:**

```
Step 1: Add column with default NULL
    -- DuckDB:
    ALTER TABLE impressions ADD COLUMN insertion_order_id VARCHAR DEFAULT NULL;
    -- ClickHouse:
    ALTER TABLE impressions ADD COLUMN insertion_order_id Nullable(String) DEFAULT NULL;

Step 2: Deploy code that populates the new column
    New events: insertion_order_id = actual value
    Old events: insertion_order_id = NULL (already in table)

Step 3: Update rollups
    Minute/hourly/daily rollups: add new dimension
    New rollups: group by insertion_order_id
    Old rollups: insertion_order_id = NULL (forward-only)

Step 4: Update report builder
    New dimension available in UI with note: "Data available from June 1, 2026"
    Queries on date ranges before June 1 exclude this dimension

Step 5 (optional): Backfill
    If the column can be derived from existing data:
    UPDATE impressions SET insertion_order_id = (
        SELECT insertion_order_id FROM line_items WHERE id = campaign_id
    ) WHERE insertion_order_id IS NULL AND timestamp > '2026-01-01';

    Re-run rollups for backfilled period.
```

**Schema evolution runbook in `migrations/ANALYTICS_SCHEMA.md`:**

```markdown
# Analytics Schema Evolution Runbook

## Adding a new column
1. ALTER TABLE with DEFAULT NULL (zero-downtime, no lock)
2. Deploy code change to populate new column
3. Update rollup configs in profiles/rollups/
4. Update report builder available dimensions
5. Decide: forward-only or backfill?

## Adding a new event table
1. Create table in DuckDB and ClickHouse (if dual-write)
2. Add to rollup framework config
3. Add NATS consumer for the new event type
4. Verify dual-write parity (if applicable)

## Removing a column
1. Stop populating in code (deploy)
2. Wait for retention period to expire (old data ages out)
3. DROP COLUMN (DuckDB/ClickHouse handle this differently)
4. Never remove a column that rollups still reference

## Parquet/Delta files
- Old files: keep as-is (immutable)
- New files: include new columns
- Delta Log: schema evolution metadata tracks column additions
- Queries across old+new files: new columns read as NULL from old files
```

### Implementation

| Component | Location |
|---|---|
| Redis budget recovery | `pkg/cache/redis/recovery.go` - NATS replay, Postgres checkpoint |
| PgBouncer deployment | `k8s/base/pgbouncer/` - primary + standby connection pools |
| Read/write split config | `pkg/store/postgres/` - `POSTGRES_URL` vs `POSTGRES_READ_URL` |
| DuckDB-ClickHouse dual-write | `pkg/store/analytics/dual.go` - writes to both, queries from primary |
| Dual-write verification job | `cmd/reporting --mode=verify-dual` - compare query results |
| Idempotent consumer | `pkg/events/consumer.go` - SetNX dedup wrapper, mandatory for all consumers |
| Fast dev mode (go run) | Tiltfile - `DEV_MODE=fast` (default) vs `DEV_MODE=container` |
| Lite local profile | `k8s/overlays/local-lite/` - minimal infra, no observability |
| Partner portal | Gateway: `/partners/*`, `/v1/openrtb/test`, `/v1/openrtb/certify` |
| Partner health check | Exchange: `/v1/openrtb/health?partner_id=xxx` |
| Clock interface | `pkg/clock/` - `Clock`, `Real`, `Fake` |
| Analytics schema runbook | `migrations/ANALYTICS_SCHEMA.md` |
| Deployment ledger | `pkg/ops/deployments.go` - see Deployment Ledger section below |

---

## Deployment Ledger

### Overview

A permanent, queryable record of every deployment event across every service. When something goes wrong at 3am, you can instantly see what version of every service was running at that exact moment and what changed.

### What Gets Recorded

Every deployment event writes a record to Postgres:

```sql
CREATE TABLE deployment_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    timestamp TIMESTAMPTZ NOT NULL DEFAULT now(),
    service TEXT NOT NULL,            -- 'dsp', 'exchange-display', 'tracker', etc.
    event_type TEXT NOT NULL,         -- 'deploy', 'rollback', 'canary_start', 'canary_promote', 'canary_rollback', 'scale', 'restart'
    image_tag TEXT NOT NULL,          -- 'ghcr.io/org/adtech-dsp:abc123f'
    git_sha TEXT NOT NULL,            -- 'abc123f'
    git_branch TEXT,                  -- 'main', 'feature/new-shading'
    previous_image_tag TEXT,          -- what was running before
    previous_git_sha TEXT,
    triggered_by TEXT NOT NULL,       -- 'ci_cd', 'manual:user@example.com', 'hpa', 'k8s_restart'
    environment TEXT NOT NULL,        -- 'local', 'staging', 'prod'
    replicas INT,                     -- how many pods after this event
    status TEXT NOT NULL,             -- 'started', 'completed', 'failed', 'rolled_back'
    duration_ms INT,                  -- how long the rollout took
    notes TEXT,                       -- optional: "deploying new bid shading algorithm"
    metadata JSONB                   -- extra: canary split %, A/B test config, etc.
);

CREATE INDEX idx_deploy_service_time ON deployment_ledger (service, timestamp DESC);
CREATE INDEX idx_deploy_env_time ON deployment_ledger (environment, timestamp DESC);
```

### How Events Are Captured

```
CI/CD deploys new DSP image:
    |
    v
GitHub Actions calls: POST /v1/api/ops/deployments/record
    {
        service: "dsp",
        event_type: "deploy",
        image_tag: "ghcr.io/org/adtech-dsp:abc123f",
        git_sha: "abc123f",
        previous_image_tag: "ghcr.io/org/adtech-dsp:def456g",
        triggered_by: "ci_cd",
        environment: "staging",
        notes: "PR #142: improved bid shading"
    }
    |
    v
K8s rollout watcher (in Gateway) detects rollout status:
    - Rollout started -> status = "started"
    - All pods healthy -> status = "completed", duration_ms = 45000
    - Pods failing -> status = "failed", auto-rollback triggered
    |
    v
Record updated with final status and duration

Canary deploy:
    record: {event_type: "canary_start", metadata: {split: "20%"}}
    ... later ...
    record: {event_type: "canary_promote"}  or  {event_type: "canary_rollback", notes: "latency regression"}
```

**Locally:** Tilt writes deployment events when it rebuilds and deploys a service. Same ledger, same format.

### Snapshot: What Was Running At Time X?

The ledger can reconstruct the exact state of every service at any point in time:

```
Query: "What was running at 2026-05-28 02:47:00 UTC?"

SELECT DISTINCT ON (service)
    service, image_tag, git_sha, timestamp
FROM deployment_ledger
WHERE timestamp <= '2026-05-28 02:47:00'
    AND environment = 'prod'
    AND status = 'completed'
ORDER BY service, timestamp DESC;

Result:
    service          | image_tag            | git_sha  | deployed_at
    -----------------+----------------------+----------+--------------------
    dsp              | adtech-dsp:abc123f   | abc123f  | 2026-05-28 02:45:00  <- deployed 2 min before anomaly!
    exchange-display | adtech-exchange:xxx  | xxx789   | 2026-05-27 14:00:00
    tracker          | adtech-tracker:yyy   | yyy456   | 2026-05-26 10:30:00
    reporting        | adtech-reporting:zzz | zzz123   | 2026-05-27 14:00:00
    ...
```

### Grafana Deployment Annotations

Deployment events are pushed to Grafana as **annotations** that appear as vertical markers on every dashboard:

```
Grafana dashboard: eCPM over time
    |
    |  $3.00  ────────────┐
    |                     │ ← deployment marker: "DSP abc123f deployed"
    |  $1.80              └──────────────
    |
    +----+----+----+----+----+----+-----> time
        02:30  02:45  03:00  03:15

Developer sees: "eCPM dropped exactly when DSP was deployed. That's the cause."
```

**How annotations work:**

```
Deployment recorded in ledger
    |
    v
Gateway pushes annotation to Grafana API:
    POST /api/annotations
    {
        dashboardUID: "*",  // appears on ALL dashboards
        time: 1716861900000,
        tags: ["deploy", "dsp", "abc123f"],
        text: "DSP deployed: abc123f (PR #142: improved bid shading)"
    }
    |
    v
Every Grafana dashboard now shows a vertical line at 02:45
    with hover tooltip showing deploy details
```

### Deployment Timeline Dashboard

Dedicated Grafana dashboard showing the full deployment history:

```
┌─────────────────────────────────────────────────────────────────┐
│  Deployment Timeline - Last 7 Days                               │
│                                                                  │
│  DSP          ──[v2.3.0]──────────[v2.3.1]──[rollback v2.3.0]──│
│  Exchange     ──[v1.8.0]────────────────────────────────────────│
│  Tracker      ──[v3.1.0]──────[v3.1.1]─────────────────────────│
│  Reporting    ──[v2.0.0]────────────────────────────────────────│
│  SSAI         ──[v1.2.0]──────────────────[v1.2.1]──────────── │
│                                                                  │
│  May 22   May 23   May 24   May 25   May 26   May 27   May 28  │
│                                                                  │
│  Deploy frequency: 2.3/day | MTTR: 4min | Rollback rate: 8%     │
│                                                                  │
│  Recent events:                                                  │
│  02:47 ⚠ DSP v2.3.1 rolled back (latency regression)           │
│  02:45 → DSP v2.3.1 deployed (PR #142)                         │
│  14:00 → Exchange v1.8.0 deployed (PR #139)                    │
│  14:00 → Reporting v2.0.0 deployed (PR #140)                   │
└─────────────────────────────────────────────────────────────────┘
```

### Deployment Metrics (DORA Metrics)

The ledger enables tracking DORA (DevOps Research and Assessment) metrics:

| Metric | Calculation | Target |
|---|---|---|
| **Deployment frequency** | Deploys per day | > 1/day (elite) |
| **Lead time for changes** | Commit timestamp -> production deploy timestamp | < 1 hour |
| **Mean time to restore (MTTR)** | Failure detected -> rollback completed | < 15 minutes |
| **Change failure rate** | Deployments that caused rollback / total deployments | < 10% |

These are calculated from the deployment ledger and displayed in the Operations UI and Grafana.

### Diff View: What Changed Between Versions?

When investigating an anomaly, you need to see exactly what code changed:

```
GET /v1/api/ops/deployments/{id}/diff

Response:
{
    service: "dsp",
    from: {git_sha: "def456g", deployed_at: "2026-05-27T14:00:00Z"},
    to: {git_sha: "abc123f", deployed_at: "2026-05-28T02:45:00Z"},
    commits: [
        {sha: "abc123f", message: "feat: improved bid shading win-rate curves", author: "alice"},
        {sha: "bcd234g", message: "fix: budget pacing rounding error", author: "bob"},
        {sha: "cde345h", message: "refactor: extract shading config", author: "alice"}
    ],
    files_changed: 8,
    lines_added: 245,
    lines_removed: 89,
    github_compare_url: "https://github.com/org/ad-tech-mono/compare/def456g...abc123f"
}
```

One click from "something went wrong" to "these are the exact code changes that caused it."

### API Endpoints

- `POST   /v1/api/ops/deployments/record` - record a deployment event (called by CI/CD, Tilt, K8s watcher)
- `GET    /v1/api/ops/deployments` - list deployment history (filter by service, env, date range)
- `GET    /v1/api/ops/deployments/snapshot?at={timestamp}` - what was running at a specific time
- `GET    /v1/api/ops/deployments/{id}` - deployment detail
- `GET    /v1/api/ops/deployments/{id}/diff` - code diff between this deploy and previous
- `GET    /v1/api/ops/deployments/metrics` - DORA metrics (deploy frequency, lead time, MTTR, failure rate)
- `GET    /v1/api/ops/deployments/timeline` - timeline data for Grafana dashboard

### Implementation

| Component | Location |
|---|---|
| Deployment ledger store | `pkg/ops/deployments.go` - Postgres CRUD for deployment records |
| K8s rollout watcher | `pkg/ops/watcher.go` - watches K8s Deployment rollout status, updates ledger |
| Grafana annotation pusher | `pkg/ops/annotations.go` - pushes deploy events to Grafana API |
| DORA metrics calculator | `pkg/ops/dora.go` - calculates deploy frequency, lead time, MTTR, failure rate |
| CI/CD integration | `.github/workflows/` - POST to /v1/api/ops/deployments/record on deploy |
| Tilt integration | Tiltfile - records deployment events on rebuild |
| Diff service | `pkg/ops/diff.go` - fetches commit list from GitHub API between two SHAs |

---

## Recent Architecture Evolution (2026-05-31)

This section captures structural changes landed in a single working session. Each is a real shift in how the platform is shaped; older sections above were updated in place where directly affected, and this section gives the cross-cutting narrative for future readers picking up the thread.

### DSPs as first-class DB entities (migration 022)

Before: a DSP's identity was a stack of (YAML profile + pod name + per-pod `dsp.profile` config row). Three places had to stay in sync. Adding a new DSP meant adding a YAML, a Tilt-launched pod, and config rows — and even then `dsp.is_competitor` was a fourth source of truth that could disagree with `noise_pct`.

After: one `dsps` table. Columns: `id (uuid), name, display_name, profile_type ('internal'|'competitor'|'external'), noise_pct, no_bid_rate, endpoint (nullable for in-cluster), status`. Plus `accounts.dsp_id UUID REFERENCES dsps(id)` so every advertiser account points at the DSP that manages it.

- Seed (`cmd/seed`) reads YAML files → INSERTs dsps rows → stamps `account.dsp_id` per campaign block
- DSP service at boot looks up its own row by name (config key `dsp.profile` becomes a name lookup, not a behavior knob): `postgres.DSPByName(ctx, db, profileName)` → uses returned `noise_pct`/`no_bid_rate` + passes `dsp.id` to the warm cache
- `CampaignLoader` filters by `accounts.dsp_id = $1` (joins accounts) — replaces the old YAML-derived `accountIDs` allowlist
- `dsp.is_competitor` config key **dropped entirely** — derived from `noise_pct > 0 || no_bid_rate > 0`. One source of truth, no more "is_competitor=true but noise=0" inconsistent state.
- Management `POST /v1/dsp/campaigns` (the create endpoint) gets-or-creates a per-DSP "default mgmt advertiser" account with `dsp_id` set to the calling DSP, so UI-created campaigns appear only in that DSP's tab.

YAML profiles stay as seed input (human-readable source of behavior+seeded campaigns); the runtime never reads them — DB is authoritative.

### Schema decentralization (migration 023 + `pkg/config.PublishSchema`)

Before: `pkg/config/Schema()` returned a hardcoded slice of 72 entries spanning every service. Adding a `dsp.foo` key meant editing a shared package. `pkg/CLAUDE.md` already said "All reusable libraries live here. Nothing is duplicated across services" — the centralized schema was inconsistent with that rule.

After: each service owns a `cmd/<svc>/config.go` declaring its own `var <svc>Schema = []config.SchemaEntry{...}`. At boot the service calls `config.PublishSchemaWithURL(dbURL, serviceName, entries, log)` which:
1. UPSERTs the rows into a new `config_schema` table (migration 023) — atomic per-service (DELETE old rows for this service + INSERT fresh)
2. Merges into the in-process registry so this service's `Validate()` and `GetDefault()` still work without a DB roundtrip

The gateway additionally calls `config.LoadPublishedSchema(ctx, db)` at boot to merge every service's rows from the table — that's how the config-manager UI sees DSP keys without importing `cmd/dsp`. `pkg/config/defaultSchema()` shrunk from 72 entries to just the platform-shared ones (server, nats, config, database, redis, s3, debug, otel, generic cache.warm).

Net: adding a new DSP knob = edit `cmd/dsp/config.go` only. The shared package never knows about service-specific keys.

### SSP `/v1/ssp/serve` — the realistic visitor path

The publisher simulator originally called the exchange and ad server directly from the browser ("X-ray mode") — useful for dev visibility but unlike any real publisher page. Real publisher pages call one SSP endpoint, get rendered HTML back, and never learn the auction winner or clearing price.

New endpoint: `/v1/ssp/serve` runs the auction (via the existing exchange call), picks the winner, calls the ad server internally, returns `{trace_id, html, impression_url, click_url, viewability_url, width, height}`. Deliberately omits winner DSP, clearing price, campaign id — competitive info that real OpenRTB never leaks to the browser.

The old `/v1/ssp/request` (returns the raw BidResponse) is preserved unchanged for tests and any dev tool that needs the X-ray view. Same `runSSPAuction` helper backs both endpoints.

Pub sim was refactored to use the new endpoint. The browser now makes one fetch + fires the SSP-supplied pixel URLs; everything else (winner identity, fan-out behavior, NATS consumer activity, win/loss notifications) lands in the trace timeline as `JAEGER` rows discovered by the Jaeger poller.

### Win/loss notification trace propagation

Before: `sendWinLossNotifications` (the goroutine that POSTs nurl/lurl to DSPs after the auction returns) used bare `client.Get(winURL)` with no context propagation. DSP-side server spans for those handlers were detached from the auction trace in Jaeger — they appeared as fresh trace_ids.

After: function takes `ctx context.Context` from `context.WithoutCancel(ctx)` (preserves trace context past handler return), opens a parent span `exchange.winloss_notify`, opens a child span per notification (`exchange.notify.win` / `exchange.notify.loss`), and uses `http.NewRequestWithContext` + `tracing.InjectHTTP` so the DSP-side `dsp GET /v1/openrtb/win` server span becomes a child of the auction trace.

Result: the full notify fan-out is visible under the auction trace in Jaeger. Exchange-side spans + DSP-side spans link cleanly.

### Per-pod warm-cache consumer collision fix

Before: all DSP pods passed `service="dsp"` to `natsbus.New`. The warm-cache invalidate consumer for `adtech.cache.invalidate.campaigns` ended up with consumer name `dsp-campaigns-cache` for every pod — JetStream load-balanced invalidates across pods (only one pod refreshed per message), leaving the others' caches stale.

After: `pkg/cache/warm.Start` appends `POD_NAME` (or `pid-{pid}` fallback) to the group: `cache.Name + "-cache-" + podID`. Each pod now gets its own consumer (`dsp-campaigns-cache-dsp-internal-0`, `...-dsp-competitor1`, `...-dsp-competitor2`) → every pod refreshes on every invalidate (broadcast semantics as intended).

This is the third instance of the "JetStream durable consumer name → semantics" pattern that's bitten us. The rule, documented in `pkg/events/natsbus.Subscribe`: include the subject in consumer name for "load-balance" semantics; include POD_NAME for "broadcast" semantics. Reporting uses the former (one pod consumes each event); warm caches use the latter (every pod refreshes).

### Pub sim becomes a publisher visitor (not a dev tool)

The publisher simulator (`/dev/publisher-simulator`) had grown into an X-ray dashboard — it showed auction winners, clearing prices, and DSP fan-out shapes because the browser was directly calling the exchange. Two cleanup passes brought it back to being a publisher visitor with all the X-ray info still available via Jaeger:

1. **Visitor mode**: refactored to call only `/v1/ssp/serve`. The browser sees HTML + pixel URLs, nothing else. Click URL is the SSP-supplied HMAC-signed one (not hand-built from internal IDs the browser no longer has).
2. **LIVE vs JAEGER badges**: each timeline row carries a source pill — `LIVE` (green) for browser-observed via fetch response, `JAEGER` (purple) for spans discovered post-hoc by the Jaeger poller. Mixing the two would have been dishonest.
3. **Jaeger poller**: after each Load Ad, the pub sim polls `GET /api/traces/{id}` every 2s for ~30s. Countdown indicator in the timeline header. New spans (NATS consumers, `exchange.notify.*`, server-side win-handler invocations) get appended as the OTel collector ingests them.
4. **Trace destinations panel**: a separate panel below the timeline shows where the trace_id should appear across the platform (Jaeger, Loki, analytics store, billing ledger, NATS, plus stubs for Postgres events / rollups / webhooks). Live counts come from `/debug/auction_wins` and `/v1/billing/ledger`. Stub rows point at PLAN.md sections describing what those systems will be when built.

The simulator also gained dev-mode scenario toggles (slow specific DSPs, trip fraud detection on impression pixel) and a full DSP/campaign management panel (per-DSP tabs, inline edit, pause/resume, delete-soft, new-campaign modal) — all driven by new CRUD endpoints on the DSP service: `POST/PATCH/DELETE /v1/dsp/campaigns` and `/v1/dsp/campaigns/{id}`. Those endpoints are real platform endpoints (not debug-only), and have explicit TODO markers for the auth middleware that needs to land before any public exposure.

### Audit log of structural changes during this session

| Change | Files touched | Notes |
|---|---|---|
| NATS consumer name = service + group + subject leaf | `pkg/events/natsbus/natsbus.go` | Fixed silent event drop in `cmd/reporting` (one consumer was overwriting another's FilterSubject) |
| Warm-cache consumer name includes POD_NAME | `pkg/cache/warm/warm.go` | Fixed broadcast semantics for cache invalidate across multiple DSP pods |
| Audience segments path A (SSP-public) + path B (DSP-private) | `migrations/019, 020`, `pkg/audience/store/postgres`, `cmd/ssp`, `cmd/dsp` | See "Audience Segment Delivery (Read Path)" above |
| `adtech.auction.win` consumed by reporting | `cmd/reporting`, `pkg/store/analytics` | Was published-and-dropped before |
| Billing accrual bug discovered + fixed | `pkg/events/natsbus`, harness UA fix | Tests now correctly verify TotalSpend (was silently passing via loose fallback) |
| `bid_timeout` enforced as fan-out context deadline + early-finish | `cmd/exchange/main.go` | Auctions complete in exactly `bid_timeout`, not "slowest DSP timeout" |
| SmartRouter EV scoring + per-channel routing | `pkg/optimise/routing.go` | Stats keyed by `(channel, dspID)`; existing EV scoring preserved |
| `dsps` table + accounts.dsp_id | `migrations/022`, `pkg/store/postgres/dsps.go`, `cmd/seed`, `cmd/dsp` | DSPs are first-class entities; `is_competitor` dropped |
| Per-service `cmd/<svc>/config.go` + `config.PublishSchema` | `migrations/023`, `pkg/config/schema.go`, every `cmd/*/main.go` | Schema decentralized; gateway loads union from DB |
| `/v1/ssp/serve` — visitor-flow endpoint | `cmd/ssp/main.go`, `pkg/routes/routes.go` | Browser-safe response shape; `runSSPAuction` helper shared with `/v1/ssp/request` |
| Win/loss notify spans + trace propagation | `cmd/exchange/main.go` | DSP-side notify handlers now linked into auction trace in Jaeger |
| Campaign CRUD on DSP | `cmd/dsp/management.go` | Real platform endpoints, NOT debug-gated, TODO for auth |
| Pub sim full overhaul | `web/templates/simulator/minimal.html` | Visitor mode, LIVE/JAEGER badges, Jaeger poller, mgmt panel, trace destinations |

### What's queued but not built (carried into next session)

- **Production auth middleware** for the DSP management endpoints (per the TODOs in `cmd/dsp/main.go` and `cmd/dsp/management.go`). Until this lands the endpoints must not be exposed to a public surface.
- **Postgres-backed billing ledger.** Schema (`ledger_entries`, `invoices`, `payouts`) exists in `migrations/`. Today's ledger is `billing.NewLedger()` — in-process, lost on restart. See "Persistence Strategy" section for the build plan.
- **DuckDB analytics store as dev default.** `pkg/store/analytics/duckdb.go` exists but `cmd/reporting/main.go:54` still uses `NewMemory()`. Selection logic via a config key. Required before any meaningful perf/load measurement.
- **CPC/CPA/vCPM settle dispatch.** `pkg/billing.Engine.ProcessEvent` supports the reserve/settle pattern but the `cmd/reporting` click/conversion/view handlers don't call it. 8 skipped `billing_models_test.go` cases gated on this.
- **External DSP Partners onboarding** — full plan in the "External DSP Partners (Multi-Tenant Integration Platform)" section earlier. Now that DSPs are first-class DB entities, external partners just need rows with `endpoint` set + auth + the partner-portal admin UI.
- **Reconciliation job** to compare analytics auction-wins against DSP budget counters (catches the small reliability gap where HTTP nurl fails and budget under-counts). Documented in "Persistence Strategy" → Implementation order step 8.

### Where to look for context in future sessions

| Topic | Section |
|---|---|
| What each DSP service owns | `cmd/dsp/CLAUDE.md`, plus the `dsps` table |
| Per-service config | `cmd/<svc>/config.go` files; gateway loads union from `config_schema` table |
| Audience segment flow | "Audience Segment Delivery (Read Path)" + memory `project_audience_segments_state.md` |
| Event dispatch + NATS consumer naming | "Consumer Naming Convention (load-bearing)" + memory `project_event_dispatch_state.md` |
| Fan-out timeouts + SmartRouter | "Exchange Fan-Out: Latency, Timeouts, and Adaptive Routing" |
| External DSP partner onboarding plan | "External DSP Partners (Multi-Tenant Integration Platform)" |
| Persistence roadmap | "Persistence Strategy" subsection in "Data Storage" |
| What's in-memory and volatile today | Same "Persistence Strategy" — state inventory table |
| Pub sim architecture | `web/templates/simulator/minimal.html` — single file, LIVE/JAEGER badges explain row provenance |

## Recent Architecture Evolution (2026-06-01)

Second working session in two days. Focus: turning the pub sim into a self-contained dev workbench (drain, reset, observability) and closing the bid-hot-path Postgres dependency that made high concurrency fail. Each structural change below is summarised; deeper details live in the referenced files.

### Audience segments moved off the bid hot path

Before: every DSP bid handler did `audienceStore.DSPSegmentsForUser(ctx, userID)` — a synchronous Postgres query per bid. Under concurrent load (drain runs, real traffic spikes) the shared connection pool queued queries past the inherited 100ms `bid_timeout`. The bid context cancelled the query, the bid path returned no-bid, and every auction in the burst came back empty. 14% fill rate at concurrency 5.

After: three-layer architecture, swappable behind one interface (`pkg/audience/store.Lookup`):

- `pkg/audience/store/postgres` — direct query, used as the fallback only.
- `pkg/audience/store/cached` — lazy Redis L2 cache. Bid path hits Redis; on miss falls back to Postgres + populates Redis with 5-min TTL. Negative results (no segments) cached too.
- `pkg/audience/store/preload` — eager warm cache. Background goroutine queries the full `audience_segment_members` table every 30s and dumps each `(user, visibility)` → segment list into Redis. Bid path is Redis-only; no Postgres on the hot path. Same pattern as the campaign/placement/creative warm caches but the backing store is shared Redis (not per-pod RAM), because audience memberships are unbounded by user count.

DSP and SSP `openAudienceStore` build the cached store when Redis is available and fall through to postgres-direct otherwise. Also added a 25ms `context.WithTimeout` around the lookup so a slow downstream never eats the bid budget — bid proceeds without segments rather than failing.

Result: 100% fill at concurrency 10 in the smoke test; previously 0%.

### Database URL bootstrap pumped into the live config

Bug discovered: every service logged `"database.url not set, ... cache will be empty"` on boot despite the schema having a default of `postgres://adtech:adtech-local-dev@localhost:5432/adtech`. Root cause: `pkg/config/Setup` resolved the DB URL into a local variable for the Postgres source connection but never wrote it back to the in-process cfg layer, so subsequent `cfg.Get("database.url", "")` calls in service handlers returned empty.

Fix: one line in `pkg/config/setup.go` — after resolving the URL, call `cfg.SetLive("database.url", dbURL)` so all subsequent reads see the same value. Restored warm-cache loading across every service.

### Prometheus metrics middleware (real histograms)

Before: hand-rolled `pkg/middleware/metrics.go` exposing a single `adtech_http_request_duration_ms` *gauge* (just an average). Useless for tail-latency analysis.

After: `prometheus/client_golang`-backed implementation with:
- `adtech_http_requests_total{service, handler, method, status}` Counter.
- `adtech_http_request_duration_seconds_bucket{...}` Histogram with bucket layout favouring sub-100ms resolution for the bid hot path (`{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000}` ms).
- `adtech_http_in_flight_requests{service}` Gauge.
- Go runtime + process collectors auto-registered.
- Same `NewMetrics(serviceName)`/`Wrap`/`Handler` API as before — no service-side changes.
- `Registry()` accessor lets services register domain-specific collectors that share the `/metrics` endpoint.
- `normalizePath()` collapses UUIDs / long IDs in URL paths to `:id` to bound label cardinality.

DSP was previously missing `/metrics` entirely — added.

### Exchange domain counters

`cmd/exchange/metrics.go` defines four collectors that populate the Pipeline Health dashboard:

- `adtech_auctions_total{result, channel}` — winner / no_bids / all_below_floor / no_winner.
- `adtech_auction_clearing_price_usd_total` — revenue counter (use `rate()` for $/sec).
- `adtech_bids_below_floor_total{placement_id}` — per-placement floor rejection.
- `adtech_bids_received_total{dsp_endpoint, decision}` — registered, increment site queued for follow-up.

All registered on the service-local registry so they emit from the same `/metrics` endpoint as the HTTP metrics.

### Grafana dashboards rebuilt against real metric names

The previously-provisioned dashboards referenced metrics that no Go code ever emitted (`adtech_auctions_total`, `adtech_revenue_usd_total`, `adtech_active_campaigns`, …). They've been rewritten:

- **Pipeline Health** — uses the new exchange domain counters (fill rate %, revenue/min, bids-below-floor) and HTTP-derived metrics (RPS, latency by DSP pod, in-flight). 13 panels.
- **Service Detail** — per-service breakdown via a `$service` template variable. Per-handler p95, error rate, goroutines + Go memory, embedded log panel.
- **All Logs** — new dashboard, single unified log stream with service / level / trace_id / search template filters.
- **Trace Explorer** — kept, simplified (dropped one Loki query using line_format syntax that doesn't fit our slog format).

On macOS the Linux-specific `process_resident_memory_bytes` isn't emitted — dashboards use `go_memory_classes_total_bytes` as the memory metric instead.

### Gateway → Jaeger CORS proxy

The pub sim polls Jaeger for spans (`GET /api/traces/{id}`) but Jaeger v1.58 doesn't support CORS on the query API. Browser fetches from the gateway-served page to `:16686` failed silently.

Fix: gateway-side reverse proxy at `routes.ProxyJaeger` (`/v1/jaeger/`) wrapping `middleware.CORS(StripPrefix(...))`. Browser hits same-origin via the gateway, the proxy adds CORS headers, traces appear in the pub sim timeline.

### `exchange.bid_timeout` default 100ms → 500ms

100ms is the IAB industry default for prod but the local Tilt stack is single-Go-process per DSP. The audience-lookup Postgres queries (before the cache fix) regularly exceeded 100ms under burst load. 500ms is more forgiving for the local-dev concurrency profile while still expressive of the real OpenRTB constraint. Live-tunable via the config manager.

### Pub sim grew into a full dev workbench

The publisher simulator (`web/templates/simulator/minimal.html`) absorbed major UX features:

- **Placement picker** dropdown replacing the old static size selector. Populated from `/v1/ssp/placements`; selecting changes (a) which placement_id is sent in the ad request, (b) the rendered ad slot dimensions, (c) which placement row shows `ACTIVE PAGE` in the SSP Inventory panel.
- **SSP Inventory panel** — collapsible publisher sections listing all placements with floor/format/size/page columns. Each placement has Floor / Pause / Delete buttons; each publisher header has a `+ Placement` button. CRUD endpoints in `cmd/ssp/management.go`.
- **DSP State refactor** — three collapsible sections (one per pod) replacing the old tabs. Header summary shows aggregate spent/remaining + drained count even when collapsed, so a `Drain budgets` run is visible across all pods at once.
- **Drain budgets modal** — configurable fixed-count vs "until exhausted" mode, concurrency, random per-batch delay. Sticky banner across the top of the page shows live progress + Stop button while the modal closes. Smart pool (built from live DSP+SSP data) filters out placements with floors no DSP can clear + geos/devices no campaign targets. Fill rate went from 14% (uniform random) to ~90% (smart pool).
- **Reset & reseed button** — POSTs `/dev/reset-and-reseed` on the gateway. Truncates 19 tenant tables → FLUSHDB Redis → re-runs `cmd/seed --profile standard` via exec → publishes NATS cache invalidates. Per-step timing surfaced in the banner.
- **Trace timeline expandable rows** — every row gets a caret; click reveals the full metadata. Browser rows show placement/visitor/trace info; SSP/tracker rows show HTTP status + URL; JAEGER rows show every OpenTelemetry tag (notify.dsp, notify.endpoint, http.status_code, auction.winner_dsp, auction.clearing_price, etc.).
- **Auction outcome banner** — pulls `auction.winner_dsp`, `auction.clearing_price`, `auction.num_bids` from the `exchange.auction` span tags (newly added in `cmd/exchange/main.go`). Shows winner + cleared price + #bidders.
- **Observability quick-link panel** — collapsible toolbar with 58 pre-built links to Prometheus queries (p95 by handler, RPS, in-flight, errors), Grafana dashboards (All Logs, Pipeline Health, Service Detail, Trace Explorer), Jaeger searches (recent auctions, bid handlers, SSP serves), Loki filters (auction outcomes, no-bids, bid submissions), debug endpoints (router stats, ledger, per-DSP campaigns), raw /metrics endpoints, and a `refreshAllCaches()` one-click action.
- **Macro substitution** — SSP now substitutes `${IMP_PIXEL}`, `${CLICK_URL}`, `${VIEWABILITY_URL}`, `${TRACE_ID}` in creative HTML before returning to the browser. Was emitting literal `${IMP_PIXEL}` which the browser tried to fetch as a URL.
- **Per-request timing** in milliseconds for sub-second events and seconds (X.YYs) for longer ones, with widened/repositioned time column so it doesn't overlap the green status line.

### `pub-simulator` is a first-class seeded publisher

The pub sim's "fake publisher page" used to use one of the other seeded publishers' placements (`pl-news-mpu`). Now there's a dedicated entry in `profiles/publishers/standard.yaml`:

```yaml
- id: pub-simulator
  name: "Publisher Simulator"
  domain: "publisher-simulator.local"
  ...
  placements:
    - id: pl-sim-mpu
      name: "Simulator MPU"
      ...
```

The pub sim defaults its placement picker to `pl-sim-mpu`. Makes the data-model link between the page and the SSP explicit: the simulator IS a publisher who's signed up with the SSP.

### Audit log of structural changes during this session

| Change | Files touched | Notes |
|---|---|---|
| Audience read path: postgres-direct → cached → preload | `pkg/audience/store/{store.go,cached/,preload/,postgres/}`, `cmd/dsp/main.go`, `cmd/ssp/main.go` | Three swappable backends behind one Lookup interface; Redis is the L2 |
| `cfg.SetLive("database.url", ...)` in Setup | `pkg/config/setup.go` | Bug fix — caches had been silently empty since the config manager landed |
| Real Prometheus histograms | `pkg/middleware/metrics.go`, `go.mod` (prometheus/client_golang dep) | Same NewMetrics API; underlying impl now emits proper buckets |
| Exchange domain counters | `cmd/exchange/{metrics.go,main.go}` | auctions_total, clearing_price, bids_below_floor |
| Gateway → Jaeger CORS proxy | `cmd/gateway/main.go`, `pkg/routes/routes.go` | Same-origin path via the gateway |
| `exchange.bid_timeout` 100ms → 500ms default | `cmd/exchange/{main.go,config.go}` | Hardcoded fallback + schema default both updated |
| Pub sim drain modal + sticky banner + smart pool | `web/templates/simulator/minimal.html` | Big chunk of JS; sample pool built from live DSP+SSP data |
| Pub sim collapsible DSP/SSP sections | same | Headers stay visible during drain so spend is watchable |
| Trace timeline expandable rows | same | Per-row metadata; ms vs Xs time formatting |
| Reset & reseed button | `cmd/gateway/{main.go,reset.go}`, `pkg/routes/routes.go` | POST /dev/reset-and-reseed: TRUNCATE + Redis FLUSHDB + cmd/seed exec + NATS invalidate |
| SSP Inventory panel + placement CRUD | `cmd/ssp/{main.go,management.go}`, `pkg/routes/routes.go` | Mirrors the DSP campaign CRUD pattern |
| Macro substitution in served HTML | `cmd/ssp/main.go` | Before serving, ${IMP_PIXEL} etc. replaced with real signed URLs |
| `pub-simulator` first-class publisher | `profiles/publishers/standard.yaml`, template default | Dedicated seed entry, default placement is its own slot |
| Auction outcome span tags | `cmd/exchange/main.go` | winner_dsp / clearing_price / num_bids exposed for the pub sim banner |
| Grafana dashboards rewrite | `k8s/base/grafana/dashboards.yaml` | All Logs (new), Pipeline Health (uses real metric names), Service Detail (per-handler p95) |
| Observability quick-link toolbar (58 links) | `web/templates/simulator/minimal.html` | Pre-built PromQL queries, Loki filters, Jaeger searches |

### What's queued but not built (carried into next session)

- **Custom domain counters in DSP** — `adtech_bids_total{dsp_id, decision="bid|no_bid"}` and `adtech_campaign_spend_usd`/`_budget_usd` gauges so Pipeline Health can show per-DSP behaviour without scraping the smart-router JSON.
- **Smart-router stats as Prometheus gauges** — currently only available via `/v1/openrtb/routing` JSON. Would give Grafana per-DSP `bid_rate` / `win_rate` over time.
- **NATS consumer lag metric** — `adtech_nats_consumer_pending_messages` referenced by the dashboard but not emitted yet.
- **CPC/CPA/vCPM settle dispatch** — `pkg/billing.Engine.ProcessEvent` supports reserve/settle but the `cmd/reporting` click/conversion/view handlers don't call it. 8 skipped `billing_models_test.go` cases gated on this.
- **Postgres-backed billing ledger** — still in-memory; loses state on restart.
- **DuckDB analytics as dev default** — code exists, just not wired.
- **Production auth middleware for DSP campaign CRUD + SSP placement CRUD** — both noted as TODO in source; must not be exposed publicly until that lands.
- **External DSP Partners onboarding plan** — full plan in PLAN.md, now blocked only on auth + per-partner metadata table.

### Local dev infra friction worth flagging

The vmType `vz` Colima setup is unstable on macOS 14 (Sonoma) — the cluster API + port-forwards drop periodically, requiring `colima start` to recover. Lima warnings flag this as a kernel/hypervisor mismatch fixed by macOS 15.5. Working around it cost real session time; macOS update is the long-term answer. `colima delete -f && colima start --vm-type qemu` is the alternative but qemu hits a separate k3s networking issue (kubelet proxy at `192.168.5.1:10250` unreachable from the API server).
