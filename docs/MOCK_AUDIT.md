# Mock / Stub Audit & Real-Data Migration Plan

> **Status:** audit complete (2026-06-29), plan not yet started. This is the
> source of truth for "what in the stack is fake and what to do about it."
> Each migration item is "done" when its named e2e test flips from `t.Skip`
> to a passing assertion (where one exists), per the suite's convention.

## How to read this

"Mocked" splits into **three categories**, and only one is a defect:

| Category | Meaning | Action |
|---|---|---|
| 🟢 **Resilience fallback** | A real implementation runs by default; an in-memory / filesystem stand-in only triggers when infra is *down*. Deliberate ("boot regardless of infra state"). | **Leave.** Revisit fail-open semantics for prod overlay only. |
| 🟡 **Intentional simulation** | Synthetic demo data by design (competitor DSPs). | **Leave.** |
| 🔴 **Genuine stub** | No real path exists — the stand-in *is* the implementation. | **Migrate to real table / API.** |

Everything in the migration plan below is 🔴.

---

## 1. Genuine stubs — inventory

Degrees: **STUB** (returns canned data / no real path) · **SIMULATED** (struct-only, fakes I/O) · **HARDCODED** (real logic, data baked into Go) · **PARTIAL** (real + missing piece).

### P0 — Data integrity (undermines "zero data slippage")

| # | Component | Degree | Evidence | Current behaviour |
|---|---|---|---|---|
| D1 | Analytics store | **PARTIAL (core+ops done)** | `cmd/reporting/analytics.go` | ✅ Backend selectable via `reporting.analytics_backend` (memory\|duckdb), mirroring `billing.ledger_backend`. With `duckdb` (build `-tags duckdb CGO_ENABLED=1`) core events **and** the 6 operational-signal events (freq-cap/render/rejection/budget/state/no-fill, via `ObservabilityWriter`) persist to a file and survive restart — proven by `TestDuckDB_SurvivesReopen` + `TestDuckDB_OperationalSignalsSurviveReopen`. Memory stays default (e2e unaffected). **Remaining:** `/debug` read-back endpoints are memory-only (501 on duckdb — only the e2e harness uses them); ClickHouse (prod, multi-replica) not implemented. |
| D2 | Rollup engine | **DONE (persist+run)** | `pkg/store/rollup/rollup.go`, `cmd/reporting/rollup.go` | ✅ Engine now maps aggregated rows → `analytics.RollupRow` and persists via `RollupWriter` (memory + DuckDB `rollups` table, JSON dims/metrics). Wired + scheduled in reporting on a minute ticker gated by `reporting.rollup_enabled` (off by default), plus `POST /debug/rollup/run?level=` for ops/e2e. Survives restart (`TestEngine_PersistsRollups`, `TestDuckDB_RollupsSurviveReopen`). **Remaining:** reporting query API doesn't yet *read* rollups by tier (`TierForRange` exists but unused); re-runs aren't idempotent (would duplicate rows). |
| D3 | Data pipeline (Parquet / Delta Log) | **PARTIAL (store done)** | `pkg/store/datalake/objstore.go` | ✅ Real `ObjectStore` writes Apache Parquet files (arrow-go, pure Go — no CGO) to object storage + a Delta-style JSON transaction log under `{table}/_delta_log/`; `Read` replays the log → active files → decodes Parquet back to Records (typed round-trip + filter). Tested against the fs object store. **Remaining:** nothing constructs it yet — `cmd/pipeline` is still health-checks-only; needs an ingest worker (watch Minio → normalise → `datalake.Write`). |

### P0 — Security

| # | Component | Degree | Evidence | Current behaviour |
|---|---|---|---|---|
| ~~S1~~ | ~~Secrets at rest~~ | **DONE** | `pkg/secrets/crypto.go` | ✅ AES-256-GCM at rest (`enc:v1:` marker), key from `SECRETS_ENCRYPTION_KEY`. Encrypt on every write (gateway create, bootstrap, seed), decrypt on warm-cache load + masked list. Passthrough when key unset (dev) and legacy plaintext rows read through, so no migration/backfill needed. |
| S2 | JWT signing / gateway auth | PARTIAL | `cmd/gateway/config.go:14`, `pkg/middleware/auth.go:29-46` | Empty signing key = dev bypass injecting `*` admin claims. Key sourced from config, not the `secrets` table. **Next up.** |

### P1 — Serving correctness & compliance

| # | Component | Degree | Evidence | Current behaviour |
|---|---|---|---|---|
| ~~C1~~ | Consent / opt-out enforcement | **DONE (DSP)** | `pkg/privacy/consent.go`, `cmd/dsp/main.go` | ✅ Opt-out registry warm cache in the DSP (`postgres.OptOutLoader` → `opt_out_registry`, NATS-invalidated), enforced on the bid path: `privacy.Evaluate` combines the registry level with OpenRTB regs (GDPR-no-consent / COPPA / US-privacy) → level 2/3 = no-bid, level 1 / reg signal = contextual-only (behavioural targeting stripped). `TestPrivacyOptOutBlocksServe` flipped from skip to a real assertion. **Remaining:** ad-server serve path doesn't check opt-outs (DSP no-bid already prevents the serve); regs-only contextual-downgrade lacks an e2e assertion (harness gap). |
| C2 | Identity graph | SIMULATED | `pkg/identity/identity.go:51-58` | Link/Resolve logic is real but in-memory only (lost on restart) and not wired into any serving path — tests only. |
| C3 | Audience segment write path | PARTIAL | `pkg/audience/store/postgres/postgres.go:1-12` | Read path fully wired; no production write API. Membership inserted only via seed / e2e raw SQL. |

### P1 — Fraud (only effective against baked-in lists)

| # | Component | Degree | Evidence | Current behaviour |
|---|---|---|---|---|
| ~~F1~~ | IP / UA blocklists | **DONE (DB-driven)** | `pkg/fraud/realtime.go`, `cmd/tracker/blocklist.go` | ✅ `fraud_blocklists` (ip/ua) loaded into a tracker warm cache (`postgres.BlocklistLoader`), pushed into the checker via `ReplaceBlocklists` on poll + `adtech.cache.invalidate.fraud-rules`. IP/UA blocks now manageable in the DB; hardcoded bot patterns + datacenter ranges retained as a floor. `TestFraudBotUARejected` flipped to a real assertion. **Remaining:** IP-block e2e needs the tracker to read `X-Forwarded-For` (reads `RemoteAddr` today); domain/app_bundle types not yet consulted. |
| ~~F2~~ | ads.txt verification | **DONE** | `pkg/fraud/adstxt.go`, `cmd/adstxt/`, `cmd/exchange/adstxt.go` | ✅ `fraud.FetchAdsTxt` (real HTTP fetch + parse, httptest-covered) + `cmd/adstxt` crawler upserting `ads_txt_cache`. Exchange warm-caches the table (`postgres.AdsTxtLoader`) and gates the auction before fan-out via `exchange.adstxt_enforcement` (off\|warn\|strict, off by default): strict → no-bid for publishers whose ads.txt omits us; `no_ads_txt` stays allowed. `TestFraudAdsTxtUnverifiedRejected` flipped to a real assertion. **Remaining:** seed the platform's own seller rows / sellers.json cross-check; `warn`-mode metric. |
| F3 | Fraud scoring | HARDCODED | `pkg/fraud/scoring.go:34-44` | Static heuristic weights/thresholds; no model. (Lowest priority — heuristic is acceptable interim.) |

### P2 — Feature completeness (code partly exists)

| # | Component | Degree | Evidence | Gap |
|---|---|---|---|---|
| X1 | Email delivery | STUB | `pkg/email/email.go:78-85` | `SMTPSender.Send()` only logs, never calls SMTP. |
| X2 | vCPM reservation expiry | PARTIAL | `cmd/reporting/main.go:390` | Settle path real; stale reservations from never-viewed impressions accumulate — expiry cron missing. |
| X3 | Ledger adjustments / refunds | STUB | `pkg/billing/tigerbeetle/ledger.go:29` | `EntryAdjustment` / `EntryRefund` rejected as unsupported. |
| X4 | Prebid SetUID | STUB | `cmd/exchange/setuid.go` | Cookie-only; no publisher→internal ID mapping. |
| X5 | Multi-imp auction dispatch | PARTIAL | `tests/e2e/competitive_auction_test.go` skip | Auction processes `imp[0]` only. |
| X6 | Pod / RelevanceWeighted / Batch auctions | STUB | `pkg/auction/strategy.go` | `ErrNotImplemented` (Phase 9 scope). |

---

## 2. Not defects — recorded so we don't "fix" them

### 🟢 Resilience fallbacks (real path runs by default)
| Integration | Real path | Fallback | Trigger |
|---|---|---|---|
| Redis L2 | `pkg/cache/redis` | `cache.NewMemoryL2()` | Redis unreachable at boot |
| Object storage | `pkg/store/objects/s3` (Minio) | `objects/fs` → `/tmp/adtech-creatives` | `s3.endpoint` empty or init fails |
| Event bus | `natsbus` | HTTP bridge to reporting | NATS / JetStream unavailable |
| Campaign / deal / contract caches | Postgres warm cache | YAML or empty set | `database.url` unset / Postgres down |
| Config live source | Postgres poll | env vars → code defaults | Postgres poll fails |

**Caveat (prod overlay, not now):** these are **fail-open** — freq-cap, dedup, and pacing silently mis-count across pods when Redis is down. Acceptable in dev; for prod, consider making Redis a readiness requirement so a pod with no shared cache drops out of rotation instead of over/under-delivering.

### 🟡 Intentional simulation
Competitor DSP `noise_pct` / `no_bid_rate` randomization (`cmd/dsp/main.go:731`) is the demo market generator. Keep.

---

## 3. Migration plan

Ordered by impact on the platform's core promise (accurate, durable data) and by blast radius. Each phase is independently shippable.

### Phase A — Durable analytics & data lake (P0 data) `[D1, D2, D3]`
The whole reporting/analytics surface is volatile today; this is the largest gap vs. "zero data slippage."

- **A1 — Activate DuckDB locally, add ClickHouse for prod.** ✅ **DuckDB done, incl. operational signals.** `reporting.analytics_backend` (`memory`|`duckdb`) + `reporting.duckdb_path`, selected by a build-tagged `selectAnalyticsStore` (`cmd/reporting/analytics{,_duckdb,_noduckdb}.go`). Core events + the 6 operational-signal events (`analytics.ObservabilityWriter`, both backends) persist + survive restart (`make test-duckdb`, `make build-reporting-duckdb`). **Still open:** (a) implement `pkg/store/analytics/clickhouse.go` (pool + batched insert) and add it to the selector for prod multi-replica; (b) route the `/debug` read-back endpoints through `Store.Query()` so they work on duckdb too (today memory-only / 501 — only the e2e harness consumes them, so low priority); (c) wire the Tiltfile/Dockerfile to build reporting with the tag if we want duckdb as the local default.
- **A2 — Persist rollups.** ✅ **Done.** `analytics.RollupWriter` + a universal `rollups` table (JSON dims/metrics) on memory + DuckDB; `rollup.runOne` maps query rows → `RollupRow` and writes them (no longer discards). Scheduling lives *inside reporting* (matches the existing `reporting.rollup_enabled` "rollup ownership centralised here" intent) on a minute ticker, with a `/debug/rollup/run` trigger — so no separate `cmd/rollup` CronJob was needed. **Still open:** reporting query API should read rollups by `TierForRange`; rollup runs need idempotency (dedupe by config+level+window) before enabling in prod.
- **A3 — Real Parquet + Delta.** ✅ **Store done.** `pkg/store/datalake/objstore.go` — `ObjectStore` writes real Arrow→Parquet to object storage + a Delta-style JSON log (`{table}/_delta_log/{version}.json`), and `Read` replays the log to decode active Parquet files back to Records (typed round-trip + filters), tested via the fs store. arrow-go is pure Go so it builds CGO-free. **Still open:** turn `cmd/pipeline` into a real worker (watch Minio → normalise → `datalake.Write`) — nothing constructs the store yet; and compaction/`remove` actions for file rewrites.
- **Done when:** reporting survives a pod restart with no data loss; `TestMigrationForward/Rollback` unblocked; a rollup query returns persisted rows.

### Phase B — Secrets & auth hardening (P0 security) `[S1, S2]`
Small, self-contained, removes the admin-bypass.

- **B1 — Encrypt secrets at rest. ✅ DONE.** `pkg/secrets/crypto.go` seals values with AES-256-GCM, stored as `enc:v1:<base64(nonce‖ct)>` in the existing `value` column (no migration). Key from `SECRETS_ENCRYPTION_KEY` (64 hex / base64, 32 bytes); unset = passthrough plaintext (dev). Wired into every write (gateway create/bootstrap/seed) and read (warm-cache loader decrypts; list endpoint decrypts before masking). Backward-compatible: values without the prefix read through as legacy plaintext, so enabling a key doesn't strand existing rows. **Prod/staging overlays must set `SECRETS_ENCRYPTION_KEY`** (KMS-sourced); a malformed key fails loud (ERROR log + `/readyz` stays red).
- **B2 — JWT key from secrets table.** Load `purpose=jwt_signing, status=active` at gateway boot; remove the empty-key dev bypass; fail boot if absent (the `cmd/gateway bootstrap` mint flow already exists). ⚠️ Riskier than B1: dev + e2e currently rely on the empty-key bypass, so this needs a seeded `jwt_signing` row + a dev token-mint path or the local stack loses auth.
- **Done when:** no plaintext secret material in Postgres; gateway refuses to start without a signing key; auth tests assert real claims (no `*` injection).

### Phase C — Privacy & identity (P1 compliance) `[C1, C2, C3]`
Logic largely exists; this is wiring + persistence.

- **C1 — Enforce consent/opt-out.** ✅ **Done (DSP).** `opt_out_registry` warm cache in the DSP (`postgres.OptOutLoader`, NATS-invalidated) + `privacy.Evaluate` gate on the bid path honouring the registry level and OpenRTB `regs`/`user.consent` (GDPR/COPPA/US-privacy). Level 2/3 → no-bid; level 1 or a reg signal → contextual-only. `TestPrivacyOptOutBlocksServe` now asserts it. **Open:** ad-server serve-path gate (belt-and-braces; DSP no-bid already blocks the serve) and an e2e assertion for the contextual-downgrade case.
- **C2 — Persist the identity graph.** Add `identity_edges` table + Redis lookup + warm cache in DSP/exchange so `Resolve()` works cross-pod at auction time.
- **C3 — Audience write API.** Endpoint/service for CRM uploads → `audience_segment_members` INSERT → publish `adtech.cache.invalidate.audience`.
- **Done when:** `privacy_test.go` opt-out + consent tests pass; identity links survive restart; a CRM upload appears in DSP targeting within one invalidation cycle.

### Phase D — Fraud to DB (P1) `[F1, F2]`
Follows the warm-cache pattern already used everywhere.

- **D1 — Blocklists to DB.** ✅ **Done.** `fraud_blocklists` (ip/ua) → tracker warm cache (`cmd/tracker/blocklist.go`) → `RealTimeChecker.ReplaceBlocklists`, refreshed on poll + `adtech.cache.invalidate.fraud-rules`. Hardcoded bot patterns/datacenter ranges kept as a floor. `TestFraudBotUARejected` asserts it; unit-tested in `pkg/fraud`. **Open:** read `X-Forwarded-For` so IP blocks are drivable e2e; consult domain/app_bundle types; optionally seed the hardcoded lists as rows.
- **D2 — ads.txt.** ✅ **Done.** `fraud.FetchAdsTxt` + `cmd/adstxt` crawler → `ads_txt_cache`; exchange warm-caches it and enforces a pre-fan-out gate (`exchange.adstxt_enforcement`, off by default). `TestFraudAdsTxtUnverifiedRejected` asserts the strict path. **Open:** schedule the crawler as a K8s CronJob; `warn`-mode rejection metric; honour DIRECT-vs-RESELLER policy.
- **Done when:** `fraud_test.go` blocklist + UA + ads.txt tests pass.

### Phase E — Feature completeness (P2) `[X1–X6]`
Pick up opportunistically; none are data-integrity blockers.

- Email → wire Mailpit (local) / SES (prod) in `SMTPSender`.
- vCPM reservation expiry cron (`Engine.Release` for >24h reservations).
- Ledger `EntryAdjustment` / `EntryRefund` when a manual-credit flow exists.
- SetUID publisher→internal ID store; multi-imp auction loop; Phase 9 auction strategies (pod/relevance/batch).

---

## 4. Test-harness debt (separate from product gaps)

Several e2e tests are skipped on **missing harness helpers**, not missing features — the billing models (tiered RS, guaranteed minimum, deal-type modifier, multi-currency) are implemented in `pkg/billing` but untestable until these land:

- `harness.ChaosKill{NATS,Redis,Postgres,Minio}` — `kubectl delete pod` helpers (unblocks 4 chaos tests).
- Contract-write helper (seed `publishers.revshare_config`) — unblocks 4 billing tests.
- Bulk-auction helper + low-TTL config knob — unblocks tiered-RS + reservation-expiry tests.
- `exchange_rates` seed + non-USD campaign — unblocks multi-currency.
- Jaeger client wrapper, single-step migration mode — unblocks observability/migration tests.

These are worth a short, separate harness pass since each unblocks multiple assertions cheaply.

---

## Appendix — correction to prior notes

The reporting consumer **does** dispatch settlement by `BidModel` (CPC→click, vCPM→viewable, CPA→conversion via `SettleByTrace`, `pkg/billing/billing.go:249`), and TigerBeetle is a real, config-selectable ledger backend (`billing.ledger_backend`, default `memory`). An earlier note that the consumer was "CPM-only" is stale and corrected here.
