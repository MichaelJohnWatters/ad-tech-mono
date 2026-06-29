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
| D1 | Analytics store | **PARTIAL (core done)** | `cmd/reporting/analytics.go` | ✅ Backend now selectable via `reporting.analytics_backend` (memory\|duckdb), mirroring `billing.ledger_backend`. With `duckdb` (build `-tags duckdb CGO_ENABLED=1`) core events persist to a file and survive restart — proven by `TestDuckDB_SurvivesReopen`. Memory stays default (e2e unaffected). **Remaining:** operational-signal tables (freq-cap/render/rejection/etc.) + `/debug` read-backs not yet on DuckDB (degrade to skip / 501); ClickHouse (prod, multi-replica) not implemented. |
| D2 | Rollup engine | PARTIAL | `pkg/store/rollup/rollup.go:156-158` | Computes aggregates then discards them — never writes a rollup table. |
| D3 | Data pipeline (Parquet / Delta Log) | SIMULATED | `pkg/store/datalake/datalake.go` | `Write()` appends to an in-memory slice and logs a fake `part-00001.parquet` path. Apache Arrow imported but unused. `cmd/pipeline` only serves health checks. |

### P0 — Security

| # | Component | Degree | Evidence | Current behaviour |
|---|---|---|---|---|
| ~~S1~~ | ~~Secrets at rest~~ | **DONE** | `pkg/secrets/crypto.go` | ✅ AES-256-GCM at rest (`enc:v1:` marker), key from `SECRETS_ENCRYPTION_KEY`. Encrypt on every write (gateway create, bootstrap, seed), decrypt on warm-cache load + masked list. Passthrough when key unset (dev) and legacy plaintext rows read through, so no migration/backfill needed. |
| S2 | JWT signing / gateway auth | PARTIAL | `cmd/gateway/config.go:14`, `pkg/middleware/auth.go:29-46` | Empty signing key = dev bypass injecting `*` admin claims. Key sourced from config, not the `secrets` table. **Next up.** |

### P1 — Serving correctness & compliance

| # | Component | Degree | Evidence | Current behaviour |
|---|---|---|---|---|
| C1 | Consent / opt-out enforcement | STUB | `tests/e2e/privacy_test.go:10,14`; `pkg/privacy/privacy.go` | Full API exists but its registry is never populated from DB and never queried at bid time. DSP/adserver have zero consent checks. |
| C2 | Identity graph | SIMULATED | `pkg/identity/identity.go:51-58` | Link/Resolve logic is real but in-memory only (lost on restart) and not wired into any serving path — tests only. |
| C3 | Audience segment write path | PARTIAL | `pkg/audience/store/postgres/postgres.go:1-12` | Read path fully wired; no production write API. Membership inserted only via seed / e2e raw SQL. |

### P1 — Fraud (only effective against baked-in lists)

| # | Component | Degree | Evidence | Current behaviour |
|---|---|---|---|---|
| F1 | IP / UA blocklists | HARDCODED | `pkg/fraud/realtime.go:64,209-237`; `pkg/fraud/lists/` is empty | Bot patterns + datacenter CIDRs baked into Go; runtime `BlockIP()` is in-memory. |
| F2 | ads.txt verification | STUB | `pkg/fraud/adstxt.go`; `tests/e2e/fraud_test.go:148` | Parser exists; no fetcher (`cmd/adstxt` does not exist), exchange never enforces. |
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

- **A1 — Activate DuckDB locally, add ClickHouse for prod.** ✅ **DuckDB done.** `reporting.analytics_backend` (`memory`|`duckdb`) + `reporting.duckdb_path`, selected by a build-tagged `selectAnalyticsStore` (`cmd/reporting/analytics{,_duckdb,_noduckdb}.go`). Core events persist + survive restart (`make test-duckdb`, `make build-reporting-duckdb`). **Still open:** (a) implement `pkg/store/analytics/clickhouse.go` (pool + batched insert) and add it to the selector for prod multi-replica; (b) bring the 6 operational-signal tables + `/debug` read-backs to DuckDB (today they're memory-only — route debug through `Store.Query()` and add the tables) so the duckdb backend has full parity; (c) wire the Tiltfile/Dockerfile to build reporting with the tag if we want duckdb as the local default.
- **A2 — Persist rollups.** Add `impressions_{minute,hourly,daily,monthly}` tables; in `rollup.runOne` upsert aggregated rows after `Query()`; schedule via a CronJob (`cmd/rollup`, to be created) at :00 / :15 / midnight / 1st.
- **A3 — Real Parquet + Delta.** Implement `pkg/store/datalake/parquet.go` (Arrow→Parquet to Minio/S3) and `delta.go` (`_delta_log/NNN.json`); turn `cmd/pipeline` into a real worker (watch Minio → ingest → validate → write).
- **Done when:** reporting survives a pod restart with no data loss; `TestMigrationForward/Rollback` unblocked; a rollup query returns persisted rows.

### Phase B — Secrets & auth hardening (P0 security) `[S1, S2]`
Small, self-contained, removes the admin-bypass.

- **B1 — Encrypt secrets at rest. ✅ DONE.** `pkg/secrets/crypto.go` seals values with AES-256-GCM, stored as `enc:v1:<base64(nonce‖ct)>` in the existing `value` column (no migration). Key from `SECRETS_ENCRYPTION_KEY` (64 hex / base64, 32 bytes); unset = passthrough plaintext (dev). Wired into every write (gateway create/bootstrap/seed) and read (warm-cache loader decrypts; list endpoint decrypts before masking). Backward-compatible: values without the prefix read through as legacy plaintext, so enabling a key doesn't strand existing rows. **Prod/staging overlays must set `SECRETS_ENCRYPTION_KEY`** (KMS-sourced); a malformed key fails loud (ERROR log + `/readyz` stays red).
- **B2 — JWT key from secrets table.** Load `purpose=jwt_signing, status=active` at gateway boot; remove the empty-key dev bypass; fail boot if absent (the `cmd/gateway bootstrap` mint flow already exists). ⚠️ Riskier than B1: dev + e2e currently rely on the empty-key bypass, so this needs a seeded `jwt_signing` row + a dev token-mint path or the local stack loses auth.
- **Done when:** no plaintext secret material in Postgres; gateway refuses to start without a signing key; auth tests assert real claims (no `*` injection).

### Phase C — Privacy & identity (P1 compliance) `[C1, C2, C3]`
Logic largely exists; this is wiring + persistence.

- **C1 — Enforce consent/opt-out.** Add a `user_optouts` warm-cache consumer; insert a `CheckConsent()` gate in the DSP bid path (before targeting) and the ad-server serve path; honour OpenRTB `regs`/`user.consent`.
- **C2 — Persist the identity graph.** Add `identity_edges` table + Redis lookup + warm cache in DSP/exchange so `Resolve()` works cross-pod at auction time.
- **C3 — Audience write API.** Endpoint/service for CRM uploads → `audience_segment_members` INSERT → publish `adtech.cache.invalidate.audience`.
- **Done when:** `privacy_test.go` opt-out + consent tests pass; identity links survive restart; a CRM upload appears in DSP targeting within one invalidation cycle.

### Phase D — Fraud to DB (P1) `[F1, F2]`
Follows the warm-cache pattern already used everywhere.

- **D1 — Blocklists to DB.** `fraud_blocklists` table (IP ranges + UA patterns) → warm cache → NATS invalidate. Seed current hardcoded lists as initial rows.
- **D2 — ads.txt.** Create `cmd/adstxt` fetcher (HTTP GET `https://{domain}/ads.txt`, 24h TTL) → `publisher_ads_txt` table; add a pre-bid enforcement gate in the exchange.
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
