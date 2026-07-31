# Conversion Attribution — Build Plan

**Status:** DRAFT for review (not yet approved to build).
**Design intent:** `docs/PLAN.md` §13 "View-Through Conversion Attribution".
**This doc:** the investigation-backed, phased implementation plan derived from
that intent. Each phase is independently shippable and e2e-verifiable.

---

## Why this exists (the gap, in one paragraph)

The live conversion path is `tracker /v1/t/conv → NATS adtech.events.conversion →
reporting.handleConversion → InsertConversion + SettleByTrace(conversionTraceID,
"conversion")`. There is **no attribution logic in that path** — the conversion
settles (or fails to) against *its own* trace id. The demo advertiser posts a
**synthetic** `tid=order-<ns>`, which references no impression or click, so a CPA
reservation is never found and nothing settles. `pkg/billing/attribution.go` is
an **unwired in-memory prototype** (last-click → view-through, 30d/7d) that nothing
imports. So: we have the events, the CPA reserve/settle machinery, the identity
graph, and a 90-day behaviour lookback — but nothing joins a conversion back to
the ad exposure that earned it.

**The join key is `trace_id`, and it must survive click → landing → conversion.**
Everything below is about establishing, widening, and privacy-hardening that join.

---

## Current-state map (verified, with file:line)

| Capability | Where | State |
|---|---|---|
| Click URL build (tid, cid, crid, pid, pubid, redir, exp, sig) | `pkg/adserving/macros.go:116-130` | ✅ signed |
| Click handler → records ClickEvent, 302 to landing | `cmd/tracker/main.go:268-330` | ✅ |
| Trace appended to landing URL as `adtech_tid` | `cmd/tracker/main.go:775-792` (`appendTraceQuery`) | ✅ (only `tid`) |
| Conversion handler (tid,type,rev,cid,crid,pid,advid,cur) | `cmd/tracker/main.go:355-414` | ✅ HMAC-gated |
| ConversionEvent type (no `user_id`, no attribution fields) | `pkg/store/analytics/analytics.go:307-320` | ⚠️ thin |
| conversions CH table (9 cols, no user_id/attribution) | `pkg/store/analytics/clickhouse.go:120-124` | ⚠️ thin |
| Conversion consumed + CPA settle | `cmd/reporting/main.go:757-784` → `SettleByTrace` | ✅ but no attribution |
| CPA settle model (reserve on impression, settle on conversion) | `pkg/billing/billing.go:614-657`, match `:772-784` | ✅ |
| behaviour_signals lookback (90d TTL, ORDER BY account,tag,user,ts) | `pkg/store/analytics/clickhouse.go:184-192` | ✅ but user_id sparse |
| Identity graph resolve/link (deterministic + probabilistic + household) | `pkg/identity/identity.go`, `household.go:23-30` | ✅ |
| Identity ingest (SSP → identity-consumer) | `cmd/ssp/identity.go:36-68`, `cmd/identity-consumer/main.go` | ✅ publisher-side only |
| Unwired attribution prototype | `pkg/billing/attribution.go` | ❌ dead code |

**Two structural gaps that gate the harder phases (Phase 2+). Phase 0/1 sidestep
both — they run purely on the deterministic `trace_id` join, no identity needed:**

1. **Advertiser-uid ↔ platform-user join is missing.** The advertiser's
   first-party id (set by `adtech-adv.js`) is never fed into the identity graph,
   so a conversion carrying only that id cannot be resolved to the platform user
   who saw the ad. *Deterministic click-through does not need this* (the trace id
   threads the link — this is what Phase 0 ships). *View-through and cross-device
   do* → Phase 2 must build the advertiser-side identity.observed feed.
2. **Impressions/clicks carry `user_id` only when the SSP observed a consented
   identity.** So behaviour_signals lookback is reliable only for consented users;
   household_id is the fallback key. Phase 2's view-through matcher must handle
   both (resolve by user_id, fall back to household_id).

---

## Phase 0 — Close the deterministic trace loop (last-click CPA) — ✅ SHIPPED (45ff9ba)

Delivered exactly as specified below and proven e2e on the live stack
(`tests/e2e/attribution_test.go`): a no-`ctid` conversion does not settle, a
`ctid` conversion settles CPA against the earning exposure + records the linkage
(`attributed_trace_id`/`attribution_type=click_through`), retries don't
double-charge. The settle fix had to land in BOTH reporting conversion consumers
(the batch path is the active one) via `ConversionEvent.SettleTraceID()`.

**Goal:** a real click → landing → conversion settles CPA against the impression
that earned it. No identity graph, no windows engine — pure deterministic join on
`trace_id`. This is the foundation everything else builds on.

**Design**
- Thread the earning trace to the advertiser and get it back on conversion:
  - `appendTraceQuery` already puts `adtech_tid` on the landing URL. Advertiser
    tag (`adtech-adv.js`) **captures `adtech_tid` from the landing URL** and
    stores it (first-party, per-visitor, with a TTL = click window).
  - On conversion, the advertiser's server postback sends that captured trace as
    a new signed param (e.g. `ctid` = "click/impression trace to credit"),
    alongside the existing synthetic order `tid` (kept as the conversion's own id
    for dedup).
- Tracker conv handler: if `ctid` present + valid, emit it as the
  **attribution touchpoint trace** on the ConversionEvent.
- Settle against the touchpoint trace, not the conversion trace:
  `SettleByTrace(ctid, "conversion")` finds the CPA reservation created at
  impression time → charges advertiser, credits the correct publisher.
- Record the linkage for reporting (conversion_trace → attributed_trace, type
  `click_through`).

**Schema / code changes**
- `analytics.ConversionEvent`: add `AttributedTraceID`, `AttributionType`,
  `UserID` (nullable).
- conversions CH table: add `attributed_trace_id`, `attribution_type`,
  `user_id` columns (migration; ClickHouse `ALTER TABLE ADD COLUMN`, default '').
- `adtech-adv.js`: capture + persist `adtech_tid`; include on the `/convert` POST.
- `cmd/demoadv/main.go`: forward captured trace as signed `ctid`.
- `cmd/reporting/main.go handleConversion`: settle against `AttributedTraceID`
  when set, else fall back to current behaviour.

**Verification (e2e)**
- Extend `scripts/advertiser-smoke.sh`: after the browser converts, assert a
  conversions row with a **non-empty `attributed_trace_id`** matching the served
  impression's trace, AND a CPA advertiser charge + publisher credit in the
  ledger (money loop already asserts spend). Zero-slippage: 1 conversion → 1
  attributed settle, no double-charge on pixel retry (dedup on conversion `tid`).

**Effort:** S–M. **Risk:** low — reuses existing reserve/settle + HMAC. **Deps:** none.

---

## Phase 1 — Attribution windows + engine (config-driven last-touch)

**Goal:** replace the "settle immediately if trace matches" shortcut with a real
last-touch engine honouring click/view windows, viewability, dedup, and
"organic = unattributed = unbilled".

**Design**
- New single-replica service **`cmd/attribution-consumer`** (patterned on
  `cmd/identity-consumer` / `cmd/webhooks`): subscribes `adtech.events.conversion`,
  runs the match, publishes **`adtech.events.attributed_conversion`** (new subject).
- Match input = behaviour_signals lookback (impressions/clicks/views for the
  touchpoint trace's user within windows). Phase 0's deterministic `ctid` remains
  the highest-priority signal; the engine adds windowed fallback + dedup.
- reporting gains `handleAttributedConversion` → `SettleByTrace(touchpointTrace,
  "conversion")`. The direct settle in `handleConversion` is removed (attribution
  becomes the single settle authority for conversions).
- Replace the dead `pkg/billing/attribution.go` in-memory map with a
  store-backed matcher (behaviour_signals query), keeping its window/priority
  semantics.

**Config** (`pkg/config/keys/reporting.go`, new `Attribution` struct, TierLive):
`attribution.enabled`, `click_through_window_days=30`, `view_through_window_days=7`,
`require_viewability=true`, `min_viewability_seconds=1.0`, `model=last_touch`.
Per-line-item overrides via the `line_items.attribution.*` JSON already documented
in PLAN.md §13 (global keys = defaults, merged at lookup).

**Verification:** e2e — conversion outside window → unattributed/unbilled;
inside window → settled; duplicate pixel → single settle; organic (no touchpoint)
→ recorded, not billed.

**Effort:** M–L (new service). **Risk:** med — moves settle authority; guard with
`attribution.enabled` kill-switch. **Deps:** Phase 0.

---

## Phase 2 — View-through + cross-device (the identity join)

**Goal:** credit conversions with no click — a viewable impression days earlier,
possibly on another device. This is where the advertiser-uid ↔ platform-user join
must be built.

**Design**
- Feed the advertiser first-party id into the identity graph: when the
  retargeting pixel fires with a uid, publish an `identity.observed` edge tagged
  as an advertiser-side signal (consent-gated), so the graph can later resolve
  advertiser-uid → platform user(s) → sibling devices.
- Conversion carries the advertiser uid; attribution-consumer resolves it via
  `identity.Resolve` to the platform user set (+ household fallback), then queries
  behaviour_signals for viewable impressions within `view_through_window_days`.
- Honour `require_viewability` / `min_viewability_seconds` (view events already
  persist server-computed IAB viewability + duration).
- Cross-device = the same resolve step returning sibling device ids; household_id
  is the fallback match key when deterministic ids don't line up.

**Verification:** e2e — impression on "device A" (no click) + direct conversion
resolving to the same user/household within window → view_through attribution;
outside window or non-viewable → none. Extend the browser smoke to a
saw-but-didn't-click path.

**Effort:** L. **Risk:** med-high (identity precision, false credit). **Deps:** Phase 1.

---

## Phase 3 — Multi-touch attribution (reporting only)

**Goal:** report how a set of touchpoints shared credit (linear / time-decay /
position-based) without changing billing (billing stays last-touch — you can't
charge a CPA twice).

**Design**
- attribution-consumer emits the full `touchpoint_chain` (already envisioned in
  PLAN.md §13's attribution record) on the attributed event.
- A reporting model apportions fractional credit per model; portals render
  MTA breakdowns. Billing continues to settle the single last-touch touchpoint.
- New CH table `attribution_touchpoints` (conversion_trace, touchpoint_trace,
  position, credit_fraction, model) for MTA reporting.

**Verification:** e2e — a 3-impression + 1-click chain → linear splits 4×0.25,
time-decay weights recent higher, last-touch still bills exactly once.

**Effort:** M. **Risk:** low (additive, reporting-only). **Deps:** Phase 2.

---

## Phase 4 — Privacy-preserving attribution (Privacy Sandbox ARA)

**Goal:** attribute without cross-site identifiers, aligned with the Privacy
Sandbox Attribution Reporting API (and the SKAdNetwork/AdAttributionKit analog).

**Design (research-heavy — spec before build)**
- Source registration: ad serve returns `Attribution-Reporting-Register-Source`
  (destination = advertiser eTLD+1, source event id, aggregation keys).
- Trigger registration: advertiser conversion returns
  `Attribution-Reporting-Register-Trigger`.
- Ingest event-level + aggregatable reports; the browser adds noise/delay, so this
  is a **separate, low-resolution** attribution stream reconciled against the
  deterministic/identity streams — not a replacement.
- Needs an aggregation-service shape (out of scope to build fully locally; stub +
  document the interface).

**Verification:** interface-level + a documented reconciliation story; full ARA
needs a real Sandbox-enabled browser.

**Effort:** L (mostly research/spec). **Risk:** high (external platform surface).
**Deps:** Phases 1–2; can be planned independently.

---

## Cross-cutting

- **NATS subject** `adtech.events.attributed_conversion` → add to
  `pkg/events/subjects.go` and PLAN.md "NATS Subjects".
- **Diagram:** money/event flow changes (new consumer, settle-against-touchpoint)
  → update the matching `docs/diagrams/*.d2` per `docs/diagrams/README.md`.
- **Multi-tenancy:** every lookback query filters `account_id` from context.
- **Privacy:** identity edges + behaviour lookback stay consent-gated
  (`privacy.Evaluate().Personalise`); organic/unconsented → no attribution, not billed.
- **New service ops:** `cmd/attribution-consumer/CLAUDE.md`, helm values, single
  replica, boot-latch/self-heal on Subscribe (per the deaf-on-boot doctrine).

## Recommended sequencing

Ship **Phase 0** first and prove the money loop end-to-end (small, high-confidence,
immediately useful). Then decide Phase 1 vs pausing. Phases 2–4 are progressively
larger and riskier; each gets its own pre-build spec pass (esp. Phase 4 ARA).
