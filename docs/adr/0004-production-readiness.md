# ADR 0004 — Production-readiness: tenant-scoped authz, privacy propagation, reporting-as-pod, day-boundary, cleanup

**Status:** Proposed (2026-07-02).
**Related:** ADR 0002/0003 (data pipeline + optimiser). Supersedes the stale `docs/PLAN.md` "Build Status & Outstanding Work" items #1/#7 with verified detail.

## Context

A three-way repo audit found the serving spine and the data/optimiser pipeline are real and wired. What remains splits into: two **production-readiness** gaps (authorization, privacy propagation), one **ops cleanup** (reporting still a local process), one **correctness** item (day-boundary runs on demo data), and **repo hygiene** (redundant/empty shells). This ADR plans them, prioritized by production-readiness × leverage.

Key correction from the audit: the DSP/SSP management endpoints are **not unauthenticated** — they already sit behind `middleware.AuthAPIKey` (`X-API-Key`). The real gap is **authorization**, not authentication:
- **No tenant scoping** — the handlers trust the account_id looked up from the path/row, not the caller's identity. A valid API key for account A can PATCH/DELETE account B's campaigns/placements (a classic IDOR / cross-tenant mutation).
- **No method-aware RBAC** — the gateway proxy applies a single `campaigns:read` permission to every method, so a mutation only needs read.
- **No audit trail** — mutations aren't logged (the `audit_log` table + write pattern exist, but only config uses them).

The building blocks already exist: `pkg/auth` (`Claims`, `CanAccessAccount`, `RolePermissions`, per-`account_type:role` permission sets), `pkg/middleware` (`Auth`, `RequirePermission`, `AuthAPIKey`, proxy that injects `X-Account-Id`/`X-User-Id`). Audit is the one new small package.

---

## P1 — Tenant-scoped authorization + audit on management CRUD (security; highest)

**Status: LANDED (2026-07-02).** `pkg/middleware.CallerScope`/`Scope.CanMutate` +
`pkg/audit.Log` wired into DSP campaign PATCH/DELETE and SSP placement CREATE/PATCH/DELETE
— account-scoped callers 403 on another account's resources; the seeded `owner='platform'`
key stays superuser (pub sim/e2e unaffected); every mutation writes an audit_log row. Unit
tests cover platform-superuser, account isolation (the IDOR), gateway-header scoping, and
deny-on-unresolved. **Deferred follow-up:** method-aware RBAC at the gateway proxy +
gateway→service credential forwarding — the JWT proxy path to DSP/SSP requires an API key at
the service today, so that path isn't fully wired independent of this fix. The direct
API-key path (how these endpoints are actually called) is now tenant-isolated + audited.

The one "don't ship without this" item. Effort: **M** (wire existing middleware + one small package).

1. **`pkg/audit`** (new) — generalize the config `audit_log` write (`pkg/config/postgres.go:113`). `audit.Log(ctx, entry)` where entry = {AccountID, ActorID, Action, ResourceType, ResourceID, Changes JSON, Timestamp}. Postgres-backed; no-op/log-only if DB unset. Actor resolved from context (API-key id via `middleware.SecretFromContext`, or claims).
2. **Tenant scoping in the handlers** (`cmd/dsp/management.go`, `cmd/ssp/management.go`) — on every mutating handler (POST/PATCH/DELETE): resolve the caller's account(s) from the injected `X-Account-Id` header (gateway path) or the API-key's account (direct path); look up the *target* row's account_id; reject with 403 unless `auth.CanAccessAccount(caller, targetAccountID)`. This closes the IDOR. Reads stay list-scoped to the caller's account.
3. **Method-aware RBAC at the gateway** (`cmd/gateway/main.go`) — split the proxy registration so POST→`campaigns:create`, PATCH→`campaigns:update`, DELETE→`campaigns:delete` (same for placements), instead of one `campaigns:read` for all. A small method-dispatch wrapper around `RequirePermission`.
4. **Audit every mutation** — call `audit.Log` after each successful create/update/delete with actor + before/after (or the changed fields).
5. **Tests** — cross-tenant PATCH/DELETE → 403; same-tenant → 200 + audit row; method→permission mapping; admin (`*`) bypass.

**Files:** `pkg/audit/audit.go` (new), `pkg/store/postgres` (audit insert), `cmd/dsp/management.go`, `cmd/ssp/management.go`, `cmd/gateway/main.go`, route/permission constants.
**Verify:** e2e — a publisher key can't mutate another publisher's placement; audit_log grows per mutation; RLS remains the backstop.

---

## P2 — Privacy opt-out ingestion + propagation (compliance)

Enforcement exists (DSP no-bids / strips segments via `privacy.Evaluate`); ingestion + fan-out don't. Effort: **M**.

1. **Write path** — `postgres.RecordOptOut(ctx, userID, level, source)` UPSERT into `opt_out_registry` (only a `LoadAll` reader exists today).
2. **Intake endpoint** — gateway `POST /v1/api/privacy/optout` `{user_id, level, source}` → `RecordOptOut` → `Publisher.OptOut` (`adtech.privacy.opt_out`, currently never published) → publish `SubjectCacheInvalidateOptOuts` so the DSP warm cache reloads immediately. Add `routes.PrivacyOptOut`. Mirror `cmd/gateway/audiences.go`.
3. **Tracker enforcement** — consume the opt-out cache (same warm-cache loader the DSP uses); reject pixels from Level ≥ 2 (NoTracking) users before recording, emitting `TrackerRejectedEvent{reason:"privacy_opt_out"}` (the reject event path already exists).
4. **Ad server (optional, same PR or follow-up)** — suppress serve / skip freq-cap writes for Level ≥ 2.
5. **Tests** — flip the skipped `tests/e2e/privacy_test.go` cases: opt-out intake → DSP no-bid within a poll; tracker rejects Level-2 pixel.

**Files:** `pkg/store/postgres/optouts.go`, `cmd/gateway/*` (new handler + route), `pkg/routes`, `cmd/tracker/*` (opt-out cache + gate), `pkg/events` (reuse).
**Verify:** POST opt-out → DSP stops bidding for that user; tracker drops their pixels with the reject reason.

---

## P3 — Reporting as a pod (ops cleanup; quick win)

Reporting only stayed a local Tilt process because DuckDB needs CGO. ClickHouse is the default now and is pure-Go, so the default build is CGO-free (`analytics_noduckdb.go` when the `duckdb` tag is absent — verified no CGO import on that path). Effort: **S**.

1. **Build CGO-free** — `GOOS=linux CGO_ENABLED=0 go build ./cmd/reporting` (no `duckdb` tag) via the shared `build/Dockerfile.dev` + `docker_build_with_restart`, mirroring the tracker pattern. Keep `build/Dockerfile.reporting` (CGO) only for the rare DuckDB-in-pod case.
2. **Deployment env** — move the env from the Tiltfile `serve_cmd` into `k8s/base/reporting/deployment.yaml`, using **in-cluster** addresses (not host port-forwards): `REPORTING_CLICKHOUSE_ADDR=clickhouse:9000`, `BILLING_TIGERBEETLE_ADDRESSES=tigerbeetle:3000`, `REPORTING_ANALYTICS_BACKEND=clickhouse`, batch + rollup on. Add `reporting` to `k8s/base/kustomization.yaml`.
3. **Tiltfile** — replace `local_resource('reporting', …)` with `reporting-build` + `docker_build_with_restart('adtech-reporting', …)` + `k8s_resource('reporting', resource_deps=['nats','postgres','clickhouse','tigerbeetle'], port_forwards=['8086:8086'])`.

**Verify:** reporting boots as a pod on clickhouse + tigerbeetle; e2e debug read-backs still work (DebugReader on ClickHouse); full e2e green. Removes the last local-process special case → clean single-image deploy.

---

## P4 — Day-boundary DB wiring (correctness)

`cmd/dayboundary` runs the right logic on **demo arrays** (`demoLineItems`/`demoIOs`); comments say "in production: Postgres queries." Effort: **M**. Schema note: **flight dates (start/end) live on `insertion_orders`, not `line_items`** — the real model is IO-driven, line-item status follows.

1. **Store methods** (`pkg/store/postgres`, all parameterized + tenant-agnostic since this is a platform job): `ActivateInsertionOrders(date)` (approved→active where start_date ≤ today), `EndInsertionOrders(date)` (active→ended where end_date < today), `DepleteInsertionOrders()` (spend ≥ budget → ended), and cascade line-item status to match their IO. Publish `CampaignStateEvent` on transitions (reporting already consumes it).
2. **Daily spend snapshot + reset** — new migration `daily_spend_snapshots(io_id, date, spend)`; snapshot before resetting the DSP's Redis daily counter (via the L2 cache). Timezone-aware: group by the IO/line-item `timezone` and compute "today" per zone.
3. **Wire** `runDayBoundary` to the store; delete the demo path (or keep behind a `--demo` flag for offline runs).

**Files:** `cmd/dayboundary/main.go`, `pkg/store/postgres` (5 methods), `migrations/NNN_daily_spend_snapshots.sql`.
**Verify:** seed an IO with past end_date → job ends it + emits state change; budget-exhausted IO → ended; snapshot row written.

---

## P5 — Repo hygiene (cleanup)

Reduce noise so "what's real" is legible. Effort: **S**. (Deletions are proposals — confirm before removing.)

- **Delete redundant shells** (logic lives elsewhere in-process): `cmd/billing` (its own CLAUDE.md says billing is unified into reporting via `pkg/billing`), `cmd/rollup` (in-process scheduler in `cmd/reporting/rollup.go`).
- **Delete pure scaffolding** (empty, no spec, no near-term plan): `cmd/cleanroom`, `cmd/ssai`, `cmd/transcoder`, and their empty `k8s/base/*` dirs; the 12 empty `k8s/cronjobs/*` dirs that have no job.
- **Keep + mark "future"**: `cmd/webhooks` (`pkg/webhooks` exists, no binary yet), `cmd/fraud` + `cmd/optimise` (batch ML jobs — real-time fraud already lives in tracker), `cmd/privacy-delete`/`privacy-verify` (GDPR Level-3 deletion — real purpose, deferred). Leave a one-line CLAUDE.md stating intent so they're not mistaken for dead scaffolding.
- **Add missing CLAUDE.md** to real-but-undocumented dirs: `cmd/publisher-adserver`, `cmd/adstxt`, `cmd/compact`, `cmd/dayboundary`, `cmd/seed`, `cmd/simulator`.

---

## Recommended order

1. **P1** (security — the only true blocker)
2. **P3** (small, high-leverage cleanup; unblocks single-image deploy)
3. **P2** (compliance)
4. **P4** (correctness)
5. **P5** (hygiene — fold in opportunistically)

Each phase is independently shippable (commit + push + tests green), same cadence as ADR 0002/0003.

## Deferred (product-gated, out of scope here)
Phase-9 auction strategies (DOOH/in-game/retail return `ErrNotImplemented`), direct CPC/CPA settle, prebid viewability-beacon injection, pubad preferred-tier + competitive exclusion. Tracked in `docs/PLAN.md`.
