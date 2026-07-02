# ADR 0005 — Plans for the ADR 0004 deferred items (reporting-as-pod, day-boundary, tracker opt-out)

**Status:** Proposed (2026-07-02). One plan per deferred item, grounded in a code/schema
investigation. **Related:** ADR 0004 (deferred these three with reasons); ADR 0003 (TigerBeetle
ledger); ADR 0002 (ClickHouse/analytics).

---

# 1. Reporting as a pod (ADR 0004 P3)

## Findings (verified)
- The blocker isn't DuckDB — it's **TigerBeetle**. `cmd/reporting/ledger.go:10` imports
  `pkg/billing/tigerbeetle` **unconditionally** (package-level), which transitively imports
  `github.com/tigerbeetle/tigerbeetle-go` (CGO). So `CGO_ENABLED=0 go build ./cmd/reporting`
  fails.
- But `tigerbeetle-go@v0.16.43` **bundles a self-contained static library**
  (`pkg/native/x86_64-linux/libtb_client.a`, Zig-compiled, `#cgo LDFLAGS: …libtb_client.a
  -ldl -lm`, no `__GLIBC` symbols) → **builds on golang:1.23-alpine + gcc + musl-dev**.
- `build/Dockerfile.reporting` **already** does exactly that: alpine + `apk add gcc musl-dev`
  + `CGO_ENABLED=1 go build ./cmd/reporting` (no `duckdb` tag → clickhouse + TB, no DuckDB).
- Build-tag precedent exists (`analytics_duckdb.go` / `analytics_noduckdb.go`).

## Two paths

**(a) In-image CGO build (RECOMMENDED).** Ship reporting as a pod via `docker_build` with the
existing `Dockerfile.reporting` — TB works, no code change. Cost: no live_update (full image
rebuild ~30–60s on reporting/pkg edits; reporting is edited rarely, and the manifest was
designed for this).

**(b) Build-tag the TB backend.** Split `ledger.go` into `ledger_tigerbeetle.go` (`//go:build
tigerbeetle`, imports tbledger) + `ledger_memory.go` (`//go:build !tigerbeetle`, memory only),
so the default build is CGO-free and can use the fast `Dockerfile.dev` + live_update pattern.
Cost: the CGO-free pod uses the **memory ledger** (volatile) unless built `-tags tigerbeetle`
— conflicts with ADR 0003-D's "durable local spend," so you'd need the tag in the local build
anyway, which reintroduces CGO. Net: (b) only helps if you're willing to run the local pod on
the memory ledger.

**Recommendation: (a).** TB is self-contained CGO, the Dockerfile exists, and it keeps durable
billing. (b) is worth it only if a CGO-free reporting image becomes a hard requirement (e.g. a
distroless prod image) — then run the pod with memory ledger + a separate TB sink, out of scope.

## Plan (path a)
1. **Tiltfile** — replace `local_resource('reporting', …)` with:
   ```python
   docker_build('adtech-reporting', '.', dockerfile='build/Dockerfile.reporting')
   k8s_yaml(['k8s/base/reporting/deployment.yaml', 'k8s/base/reporting/service.yaml'])
   k8s_resource('reporting', resource_deps=['nats','postgres','clickhouse','tigerbeetle'],
       port_forwards=['8086:8086'], labels=['services'])
   ```
   (No `reporting-build` local_resource, no live_update.)
2. **Deployment env** — already prepped (ADR 0004 commit): in-cluster addrs `clickhouse:9000`,
   `tigerbeetle:3000`, `redis:6379`, backend=clickhouse, batch + rollup on. Verify no host-only
   env leaks (POD_NAME from fieldRef is fine).
3. **Dockerfile.reporting** — confirm it builds without the `duckdb` tag (it does) and add a
   `.dockerignore`/context note so the full-repo build context isn't huge.

## Verify
- `tilt up` → reporting pod reaches Ready; `/readyz` green; consumes NATS; ClickHouse rows +
  TB ledger balances present.
- e2e debug read-backs still work (DebugReader on ClickHouse); full e2e green.
- Iteration cost acceptable (image rebuild on reporting change).

## Effort / risk
**S–M.** No code change; the risk is purely "does the in-image CGO build + TB-in-pod connection
work," which needs a live `tilt up` (can't verify in-session). Reversible: revert the Tiltfile
block to `local_resource`.

---

# 2. Day-boundary lifecycle job (ADR 0004 P4)

## Findings (verified — the demo model doesn't match the schema)
- **`line_items` has NO `start_date`/`end_date`** (migration 005). Flight dates live **only on
  `insertion_orders`** (migration 004: `start_date DATE`, `end_date DATE`). The demo's
  per-line-item flight fields are fiction.
- **Status vocab differs:** IO ∈ {draft, active, paused, ended, archived} (**no "live"**);
  line_item ∈ {draft, submitted, in_review, rejected, approved, live, paused, ended, archived}.
- **Bids gate on `line_item.status == 'live'` only** (`cmd/dsp/main.go:751`,
  `pkg/store/postgres/campaigns.go:100`). IO status does **not** gate bids, and the FK has **no
  cascade**. So **ending an IO must explicitly flip its line_items to `ended`** or bids continue.
- **Spend is not in Postgres.** Runtime daily spend is Redis (`dsp:budget:{lineItemID}:spent`,
  cents, daily TTL, `cmd/dsp/budget.go:33`); historical is `ledger_entries` (migration 012). No
  `daily_spend_snapshots` table exists.
- `CampaignStateEvent` is line-item-centric (needs campaign_id + account_id); dayboundary can
  publish it per line-item transition (reporting already consumes it).

## Design (IO-driven flights, explicit line-item cascade)
Model flights at the IO (where the dates are); line-item status follows its IO. Phase it so the
high-value, low-risk part lands first without a migration.

### Phase 1 — Flight transitions (no migration, no Redis) — do first
Store methods on `pkg/store/postgres` (platform job, no tenant filter), all parameterized:
- **Activate**: IOs `draft→active` where `start_date <= today AND end_date >= today`; then
  cascade their line_items `approved→live`. Return the affected (line_item_id, account_id) set.
- **End (flight)**: IOs `active→ended` where `end_date < today`; cascade line_items
  `live→ended`.
Publish `CampaignStateEvent` per cascaded line_item + `adtech.cache.invalidate.campaigns` so the
DSP warm cache reloads and stops bidding. Wire `runDayBoundary` to call these; delete the demo
path (or keep behind `--demo`).

### Phase 2 — Daily snapshot + budget reset (migration + Redis)
- **Migration** `NNN_daily_spend_snapshots.sql`: `daily_spend_snapshots(line_item_id UUID,
  snapshot_date DATE, spend DECIMAL, daily_budget DECIMAL, created_at, PRIMARY KEY
  (line_item_id, snapshot_date))`. (Migrations run at `make migrate` boot — test locally before
  merge.)
- dayboundary gains a **Redis client**: for each live line_item, read `dsp:budget:{id}:spent`,
  write a snapshot row, then reset the counter (delete key). Order: snapshot **before** reset.

### Phase 3 — IO budget depletion
- Sum each active IO's line-items' Redis spend; if `>= io.budget`, end the IO + cascade
  line_items `live→ended` + publish. (Redis is the authoritative runtime spend.)

### Phase 4 — Timezone correctness
- line_items carry `timezone`; compute "today"/midnight per line_item timezone rather than a
  single UTC day, so a campaign ends on *its* local midnight. IOs have no timezone → use the
  line-item's (or account default).

## Files
`cmd/dayboundary/main.go` (wire to store + redis, drop demo), `pkg/store/postgres/dayboundary.go`
(new: Activate/EndFlights/Deplete methods returning affected line-items), `migrations/NNN_daily_
spend_snapshots.sql` (Phase 2), reuse `events.Publisher.CampaignStateChanged` + budget Redis keys.

## Verify
Seed an IO with a past `end_date` → job ends it, flips its line_items to `ended`, emits state
events, DSP stops bidding after invalidate. Budget-exhausted IO (Redis spend ≥ budget) → ended.
Snapshot row written before the Redis reset. (Integration/e2e — the SQL + Redis need a live DB.)

## Effort / risk
**M–L.** Phase 1 is low-risk (existing schema, standalone job, no hot path). Phases 2–4 add a
migration (boot-time risk — test first) + Redis + timezone. Also: `cmd/dayboundary` isn't wired
as a CronJob yet, so schedule it (k8s/cronjobs) as part of Phase 1 or it never runs.

---

# 3. Tracker opt-out enforcement (ADR 0004 P2 remainder)

## Findings
- Tracker pixels are keyed by `tid` (trace_id) + `event` (`cmd/tracker/main.go:450`), with **no
  user id**. Adserver pixel URLs carry no uid either.
- The **DSP already gates upstream**: an opted-out user is no-bid / segment-stripped before an
  ad is ever served, so no pixel fires for them in the normal flow.

## Recommendation: DON'T build it unless a uid lands in pixels for another reason.
Tracker-side rejection is redundant defense-in-depth, and the only way to do it is to add a
user identifier to signed pixel URLs — which **exposes user ids in URLs/access logs**, a privacy
regression that cuts against the platform's "hash PII, no uid in the clear" posture.

## Conditional plan (only if pixels gain a hashed uid for other reasons)
1. Adserver: include a **salted-hashed** uid (`h = HMAC(uid)`) in the signed pixel URL — never
   the raw uid.
2. Tracker: load an opt-out warm cache (reuse `postgres.OptOutLoader` + `warm.Cache`, exactly as
   `cmd/dsp/startOptOutCache`), keyed by the same hash.
3. In each pixel handler, after HMAC verify + fraud check, if the hashed uid maps to opt-out
   Level ≥ 2 → drop + `publishRejected(reason="privacy_opt_out")` (the reject path exists).
Effort **S** once the hashed uid exists; **not worth** adding the uid solely for this.

---

## Suggested order
**Reporting-pod (1)** — smallest, unblocks single-image deploy, needs only a `tilt up` check.
Then **day-boundary Phase 1** (flights) as its own change + schedule the CronJob. Defer
day-boundary Phases 2–4 and the tracker gate until there's a concrete need.
