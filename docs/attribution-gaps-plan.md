# Attribution — Gaps Plan

**Status:** DRAFT for review. The attribution pipeline (Phases 0–3 in
`docs/attribution-plan.md`) works and is e2e-proven. This plan covers the
"make it self-serve / multi-tenant-real" gaps found when auditing what config &
integration each side actually needs. Grounded in a current-state investigation
(file:line below).

## The gaps at a glance

| # | Gap | Side | Effort | Risk | Value | Status |
|---|-----|------|--------|------|-------|--------|
| G1 | Publisher SDK forwards `hashed_email` | publisher | XS | low | completes the identity bridge | ✅ shipped |
| G2 | Browser demo bridges end-to-end | demo | S | low | visible proof, no seeding | ✅ shipped |
| G3 | Tenant-scope `/v1/reporting/attribution` | platform | S | **security** | prerequisite for G4 | ✅ shipped |
| G4 | Portal attribution / MTA view | platform UI | M | low | the visible payoff | ✅ shipped |
| G5 | Per-line-item attribution overrides | platform | M | low | per-campaign windows/model | todo |
| G6 | Household `hh` last-mile on the beacon | platform | S | low | CTV/cross-device fallback | ✅ shipped |
| G7 | Per-advertiser signing keys | platform | L | **security** | true multi-tenant + anti-fraud | todo |

**G1–G3 shipped.** Key find while doing G1: `ssp.identity_observe_enabled`
defaulted to **false**, so the SSP never built identity edges from serves — the
publisher half of the bridge was dead regardless of the SDK. Turned it on in the
chart (values.yaml, `SSP_IDENTITY_OBSERVE_ENABLED=true`). G3 also self-enforces
the account filter in the reporting handler (defense in depth), not just at the
gateway, because the open `ProxyReporting` passthrough exists. e2e:
`tests/e2e/attribution_gaps_test.go` (serve→publisher edge; owner-only chain read).

Recommended order: **G1 → G2** (finish the bridge, make the demo real), then
**G3 → G4** (secure + surface the data), then **G5 / G6** (depth), then **G7**
(the big multi-tenant/security lift) as its own effort.

---

## G1 — Publisher SDK forwards `hashed_email` (XS)

**Now:** `adtech.js setUserData({hashedEmail})` stores it in `state.userData`
(web/static/adtech.js:132-139) but `requestAd()` builds the serve params with
only `placement_id`/`user_id`/`geo`/`device` (adtech.js:159-166) — **it never
sends `hashed_email`**. So the publisher half of the identity bridge (the SSP
observing `publisher_user_id ↔ hashed_email`) never fires.

**Design:** one change — in `requestAd()` add
`if (state.userData.hashedEmail) params.set('hashed_email', …)`. The rest of the
path already carries it: publisher-adserver `forwardSSPQuery()`/`callSSP()` copy
ALL incoming query params onward (cmd/publisher-adserver/main.go:546-584), and
the SSP's `gatherSignals()` already reads `q.Get("hashed_email")`
(cmd/ssp/identity.go:50) and publishes the identity observation. Do the same for
the video/native/audio request builders for parity.

**Verify:** e2e — a serve carrying `hashed_email` produces an
`identity_graph` edge `publisher_user_id ↔ hashed_email` (assert via DB), i.e.
the publisher half the Phase-2 bridge test currently seeds.

**Deps:** none. **Risk:** low (consent already gates identity writes).

---

## G2 — Browser demo bridges end-to-end (S)

**Now:** the platform bridge is proven in e2e but the live browser demo still
needs both halves to share a real hashed email. demoadv has `setEmail()` (G-side
shipped); demosite doesn't set one, and doesn't call `setUserData({hashedEmail})`.

**Design:** give the demo a shared persona email. demosite (cmd/demosite) hashes
`persona@example.com` → `adtech.setUserData({hashedEmail})` before `requestAd`;
demoadv already hashes the same email via `setEmail`. With G1, the SSP observes
the publisher edge and the tracker (via the retargeting pixel) observes the
advertiser edge → the graph unifies → a browser-only visit→see-ad→convert
attributes view-through with zero seeding. Wire a tiny "same person" toggle/email
in both demo sites.

**Verify:** extend the browser smoke (chromedp) OR document a manual runbook:
visit demosite (see ad), visit demoadv (convert), check `attribution_type=
view_through` in ClickHouse. **Deps:** G1. **Risk:** low; browser-flaky (smoke only).

---

## G3 — Tenant-scope `/v1/reporting/attribution` (S, security)

**Now:** the endpoint (cmd/reporting/attribution_api.go) takes a
`conversion_trace` and returns its chain with **no account check**, and the
gateway proxies it via the pass-through `routes.ProxyReporting` (open CORS proxy,
cmd/gateway/main.go:845). An advertiser could read **another advertiser's**
conversion chain by guessing/lifting a trace id.

**Design:** mirror the reports path (cmd/gateway/reports_scope.go
`enforceReportTenant`): the endpoint must filter `attribution_touchpoints` by the
caller's `account_id` (from `X-Account-ID`/`X-Account-Type`), and the gateway
route must go through `RequirePermission("reports:read")` + a scope check, not the
open proxy. The chain rows already carry `account_id`, so add an `accountID`
filter to `AttributionChain` (and reject/empty when it doesn't match).

**Verify:** e2e — advertiser A cannot read advertiser B's conversion chain (403 /
empty); A can read its own. **Deps:** none. **Risk:** it's a real hole — do it
before G4 exposes the endpoint in the UI.

---

## G4 — Portal attribution / MTA view (M)

**Now:** the MTA data + read API exist; nothing renders them. The portal is a
single-page HTMX/JS shell (cmd/gateway/portal.go, web/templates/portal/
advertiser.html) with per-feature sections + JS loaders; the Reports section
(advertiser.html:245-313) is the pattern to mirror.

**Design:** add an "Attribution" nav item (portal.go advertiser nav, perm
`reports:read`) + a `#attribution` section (trace-id input + model picker +
results table) + a `queryAttribution()` JS loader that fetches the (now
G3-scoped) endpoint and renders per-touchpoint credit. No new gateway handler —
the scoped route from G3 is the source.

**Verify:** portal loads a conversion's chain, switching model re-apportions.
**Deps:** G3. **Risk:** low (read-only UI).

---

## G5 — Per-line-item attribution overrides (M)

**Now:** attribution windows/model are **global** (keys.Attribution.*, read in
cmd/reporting/attribution.go). PLAN.md §13 envisioned per-line-item
`attribution.*`. Reporting doesn't query line_items today, but it has the lazy
per-entity PG-cache pattern (cmd/reporting/rates.go, dealtypes.go: lazy DB +
bounded cache + 3s timeout + fail-open).

**Design:** add `attribution_config JSONB` to the `targeting_rules` companion
table (migrations; the 1:1 per-line-item table that already holds `bid_modifiers`/
`frequency_caps` JSONB). A new `pgAttributionConfigSource` (mirror dealtypes.go)
lazily loads a campaign's overrides, cached + fail-open to the global keys. The
attributor merges per-campaign over global when `e.CampaignID` is set. UI: a small
attribution block on the line-item editor (later; optional).

**Verify:** e2e — a campaign with a 1-hour view window doesn't attribute a
2-hour-old exposure that the global 7-day window would. **Deps:** none (global
keys are the fallback). **Risk:** low; additive.

---

## G6 — Household `hh` last-mile on the beacon (S)

**Now:** the view-through matcher + `publishBehaviour` now support household
matching (unit-tested), but the impression beacon never carries `hh`, so
`behaviour_signals.household_id` is always empty — the fallback has no data.

**Design:** the ad server bakes the resolved household id onto the served
beacon(s) (`hh=` param) the same way it bakes the consented `uid`
(models.ServeRequest.BehaviourUserID → beacon). Then `publishBehaviour` (already
reads `q.Get("hh")`) records it, and household-linked exposures match.

**Verify:** e2e — an exposure recorded under a household matches a conversion
whose visitor resolves (via a `uid ↔ household` edge) to that household.
**Deps:** none. **Risk:** low.

---

## G7 — Per-advertiser signing keys (L, security + multi-tenant)

**Now:** ONE shared key signs/validates everything (pkg/adserving/signing.go,
rotation.go `ActiveSigningKey`); the tracker validates against the whole platform
key set (`sigKeys()`, cmd/tracker/main.go:128-134, `ValidateSignatureAny`). The
`secrets` table is `owner='platform'` with no `account_id` (migrations/027);
`accounts` has no key column. demoadv uses `DefaultSigningKey`. **So any party
holding the shared key can forge a signed conversion for ANY advertiser** — the
CPA billing-fraud surface Phase 0 was careful about is only half-closed.

**Scope note:** platform-signed beacons (impression/click/view, signed by OUR ad
server) can stay on the platform key. The keys that should be **per-advertiser**
are the ones the ADVERTISER holds and signs: the S2S **conversion** postback (and
the retargeting pixel). So this is really "issue each advertiser its own
conversion-signing key, and validate `/v1/t/conv` (+ `/v1/t/rt`) against that
advertiser's key by `advid`."

**Design:**
- Schema: add `account_id` (nullable) to `secrets` (null = platform-wide, current
  behaviour); a per-advertiser row with `purpose='hmac_conversion'`.
- Issuance: an advertiser portal action + gateway API to generate/rotate an
  advertiser's key (reuse the active/rotating/revoked lifecycle already in
  secrets); surface it once for the advertiser to configure their server.
- Validation: the tracker's conversion/retarget handlers look up
  `sigKeysForAdvertiser(advid)` = the advertiser's key set (+ platform fallback
  during migration) instead of the global set.
- demoadv: fetch its issued key (env/config) instead of `DefaultSigningKey`.
- Migration: backfill existing advertisers to the platform key so nothing breaks;
  flip to strict per-advertiser once keys are issued.

**Verify:** e2e — advertiser A's key signs a conversion for A (settles);
A's key signing a conversion for B's campaign is REJECTED.

**Deps:** none, but it's the largest change (schema + issuance UI/API + validation
+ demo). **Risk:** security-sensitive; stage behind a config flag + platform-key
fallback so a bad rollout can't reject real conversions.

---

## Not in scope here

- Full Privacy Sandbox / ARA (Phase 4 in `docs/attribution-plan.md`) — separate,
  research-heavy.
- Click-through-only conversions still don't carry a chain unless a `uid` is
  present (documented in the Phase-3 follow-up).
