# Adtech Standards — Coverage & Roadmap

**Status:** living document · **Last audited:** 2026-07-05 · **Last built:** 2026-07-06

This is the single source of truth for **which industry adtech standards this
platform implements, how well, and what we plan to build next.** Update the
status columns as work lands. See `docs/PLAN.md` for the broader project plan
and `CLAUDE.md` for coding conventions.

## Why we track this

Adtech is heavily standardised — mostly by **IAB Tech Lab** (technical specs)
and **IAB / IAB Europe** (policy + consent). Because these specs are public,
stable, and widely published, they are the highest-leverage things to build:
the blueprint is unambiguous, interop is well-defined, and correctness is
verifiable against a written spec rather than guesswork. We prioritise standards
by **industry prominence × how close we already are × spec clarity.**

**Prominence legend:** 🔴 universal/table-stakes · 🟠 high but shifting ·
🟡 medium/growing · 🟢 niche
**Our status legend:** ✅ solid (production-grade) · 🟨 partial (schema/signal
only, no runtime) · ⬜ not implemented

---

## Where we are (2026-07-06 build-out)

A build-out pushed most of the roadmap from "planned" to "shipped." Every
headline standard is now implemented end-to-end (request → auction → serve →
measure), and the transparency/trust stack is complete. Summary of what landed
and what's genuinely left; the tables and roadmap below have the per-standard
detail, and the change log at the bottom has commit-level notes.

### Shipped

- **Transparency & trust triad + signing.** ads.txt, app-ads.txt (crawler +
  `app_ads_txt_cache`), sellers.json, **SupplyChain (schain)** (SSP originates →
  exchange validates), and **ads.cert** — Ed25519 signed bid requests
  (`pkg/adcert`) with a signed timestamp for replay protection, the exchange
  publishing its key at `/v1/adcert/key`, and DSPs fetching + refreshing it.
- **Privacy.** The SSP now actually populates `Regs`/consent on the wire (it
  didn't before — enforcement ran on empty inputs). First-class **GPC**
  (`Regs.ext.gpc`), **GPP** US-National opt-out decode (`pkg/privacy/gpp.go`),
  and `Evaluate` refactored to a `privacy.Signals` struct. TCF/USP/GDPR/CCPA/COPPA
  already existed.
- **Native (OpenRTB Native 1.2).** Full lifecycle: `pkg/native` markup, SSP
  request, `creatives.native_assets` (mig 036) + DSP native match/response, and
  publisher-adserver HTML rendering with signed trackers (`/v1/pubad/native`).
- **Video / OMID.** Server-side Open Measurement — `<AdVerifications>` (OM SDK
  script + not-executed beacon) emitted in VAST behind
  `publisher_adserver.omid_verification_url`; SSP signals `api:[7]`.
- **Classification.** Complete IAB Content Taxonomy 1.0 (`pkg/taxonomy`, all 26
  tier-1) + validation/lookup/parent-resolution; fixed a real IAB10/IAB21 bug.
- **Identity / UID2.** UID2 carried in `User.EIDs` and used as a stable user key;
  **the identity graph is now functional end-to-end** — it **auto-builds** from the
  request stream (the SSP observes co-occurring identifiers — user_id, uid2,
  hashed_email, ifa — and writes deterministic edges off the hot path, batched +
  deduped; `ssp.identity_observe_enabled`), also takes explicit uploads
  (`/v1/api/identity-links`), and the DSP **resolves** a user to linked ids for
  segment lookup off an **in-memory preload** (refreshed every
  `dsp.identity_preload_interval`, default 5m; interned ids) so the bid path never
  hits Postgres — QPS-safe, sized for a few million ids. Resolution is
  **transitive** (bounded BFS, `dsp.identity_max_depth`) and **confidence-gated**
  (`dsp.identity_min_confidence`), so uid2→email→device all resolve together.
- **Docs.** `docs/openapi.yaml` refreshed (BidRequest fields + new endpoints).

### What's next (prioritized, with honest caveats)

1. **Identity auto-build — deepen it.** Deterministic + probabilistic auto-build,
   now **event-driven**: the SSP publishes per-request identity signals
   (`adtech.identity.observed`), and `cmd/identity-consumer` (using
   `pkg/identityobserve`) batches/dedupes and writes edges — write is off every
   serving pod and the probabilistic fingerprint state is one global view (run a
   single replica). The **exchange** now also observes (inbound Prebid demand our
   SSP never saw, `exchange.identity_observe_enabled`) via the shared
   `identityobserve.Publisher`. Fuzzy UA matching (`identity_consumer.fuzzy_ua`)
   and Redis-backed fingerprint buckets (`identity_consumer.redis_url`, enables
   multi-replica) are done. Only remaining: observe on the **tracker** — but its
   URLs carry no user identity today, so it would need user ids plumbed into the
   signed tracker URLs (privacy-sensitive), which is why it's parked, not minor.
2. **GPP US state sections 8–12** (US-CA/VA/CO/UT/CT opt-out decode). *Caveat:*
   each has a distinct bit-layout and we have no official IAB test vectors, so
   correctness can't be verified — deferred deliberately (decode is downgrade-only
   / safe, but shipping guessed offsets is a quality risk). Same caveat already
   noted on US-National.
3. **UID2 operator integration** — real UID2 token *decryption* + key rotation
   via a UID2 operator. Needs the external operator service/keys; we currently
   treat the token as an opaque stable id.
4. **Identity graph scaling levers** (only if it grows past a few million):
   delta refresh (load only changed edges), integer-id adjacency, or a dedicated
   identity store. *(Transitive multi-hop resolution + confidence gating: done.)*
5. **Client-side OM SDK runtime + SIMID interactive creative.** Out of scope for
   a Go repo — needs a JS measurement/interactive runtime we don't have.
6. **ads.cert multi-key rotation** — carry a key id in the signed request so the
   exchange can publish old+new keys during a rotation window (single-key today).

---

## 1. Bidding & Auction Protocols

| Standard | Steward | Who it's for | Prominence | Our status | Where / notes |
|---|---|---|---|---|---|
| OpenRTB 2.6 | IAB Tech Lab | SSP/DSP/exchange | 🔴 | ✅ | `pkg/openrtb/openrtb.go` — full BidRequest/SeatBid/Imp/Video/Audio/Native/Regs |
| OpenRTB Native 1.2 | IAB Tech Lab | Native placements | 🔴 | ✅ | Fully end-to-end: `pkg/native` markup, SSP native request + winner short-circuit, `native_assets` (mig 036) + DSP native match/response, and publisher-adserver `nativeHandler` (`/v1/pubad/native`) renders the markup to an HTML card with signed impression/click trackers |
| Prebid Server / JS | Prebid.org | Publishers, header bidding | 🔴 | ✅ | `cmd/exchange/prebid.go`, bidder code `adtechmono`, `/setuid` |
| Deals / PMP / PG (OpenRTB deal object) | IAB Tech Lab | Direct deals | 🔴 | ✅ | `pkg/deals/` matcher (PG/Preferred/PMP/Open priority) |
| OpenRTB 3.0 / AdCOM | IAB Tech Lab | Next-gen exchanges | 🟡 | ⬜ | Deprioritised — 2.x dominates |

## 2. Supply-Chain Transparency

| Standard | Steward | Who it's for | Prominence | Our status | Where / notes |
|---|---|---|---|---|---|
| ads.txt | IAB Tech Lab | Web publishers | 🔴 | ✅ | crawler `cmd/adstxt/`, parser `pkg/fraud/adstxt.go`, enforced at exchange |
| sellers.json | IAB Tech Lab | SSP/exchange | 🔴 | ✅ | `cmd/gateway/sellers.go`, backed by publishers table |
| **SupplyChain Object (schain)** | IAB Tech Lab | Every RTB hop | 🔴 | ✅ | SSP originates `Source.Ext.schain` (`cmd/ssp` `originSChain`); exchange validates behind `exchange.schain_enforcement` (`cmd/exchange/schain.go`); types + `ValidateSChain` in `pkg/openrtb/schain.go` |
| app-ads.txt | IAB Tech Lab | App publishers | 🔴 | ✅ | crawler `cmd/appadstxt/` + `pkg/fraud/appads.go` (`FetchAppAdsTxt`), cache `app_ads_txt_cache` (migration 035). Enforcement/warm-cache = follow-up |
| ads.cert 2.0 (signed bids) | IAB Tech Lab | Exchange/DSP anti-spoof | 🟡 | ✅ | `pkg/adcert` Ed25519 sign/verify over canonical fields + signed timestamp (replay protection); exchange signs (`Source.Ext.adcert`) **and publishes its key at `/v1/adcert/key`**; DSP verifies + enforces freshness and **fetches/refreshes the key** (`dsp.adcert_key_url`), falling back to a static key |

## 3. Video / Audio / CTV

| Standard | Steward | Who it's for | Prominence | Our status | Where / notes |
|---|---|---|---|---|---|
| VAST 4.2 | IAB Tech Lab | Video ad serving | 🔴 | ✅ | `pkg/vast/vast.go`, `cmd/publisher-adserver/vast.go`, full tracking events |
| VAST 4.2 §6 macros + §2.3.6.3 error codes | IAB Tech Lab | Player-substituted URI macros | 🔴 | ✅ | `[ERRORCODE]/[CACHEBUSTING]/[TIMESTAMP]/[ADPLAYHEAD]` emitted as unsigned params after the HMAC (`adserving.AppendClientMacroParams`); tracker validates via `mediaSigParams` filter, stores `media_events.error_code` (literal macro → 900 per spec); player half in `pkg/vast/macros.go` (sim) + `adtech.js` 2.1.0 |
| VMAP 1.0 | IAB Tech Lab | Ad break scheduling | 🔴 | ✅ | `cmd/publisher-adserver/vmap.go` — pre/mid/post-roll, independent auctions |
| OMID / Open Measurement | IAB Tech Lab | Viewability/verification | 🔴 | 🟨 | Server-side done: SSP signals `api:[7]` OMID, publisher-adserver emits `<AdVerifications>` (OM SDK script + signed verificationNotExecuted beacon) in VAST via `pkg/vast`, gated by `publisher_adserver.omid_verification_url`. Client-side OM SDK runtime (session JS) is out of scope (no OM SDK in-repo) |
| SIMID 1.1 | IAB Tech Lab | Interactive video (VPAID successor) | 🟡 | 🟨 | Struct in `pkg/vast`; no runtime |
| VPAID 2.0 | IAB Tech Lab | Interactive video (legacy) | 🟠 | 🟨 | Signal codes only. **Deprecated — skip, go to SIMID+OMID** |
| VAST audio / DAAST | IAB Tech Lab | Podcast/streaming audio | 🟡 | 🟨 | Audio object + DAAST protocol codes; no dedicated pipeline |
| SSAI (server-side ad insertion) | pattern | CTV/streaming | 🔴 | 🟨 | VMAP breaks auction independently; no stitching layer |
| DOOH / OpenOOH | IAB Tech Lab / OpenOOH | Out-of-home | 🟡 | ⬜ | Stubbed in `pkg/auction/timeslot.go` (TODO) |

## 4. Privacy & Consent

| Standard | Steward | Who it's for | Prominence | Our status | Where / notes |
|---|---|---|---|---|---|
| IAB TCF v2.2 (GDPR) | IAB Europe | EU traffic | 🔴 | ✅ | `pkg/privacy/consent.go`, evaluated at bid time |
| US Privacy String (CCPA) | IAB Tech Lab | US traffic (legacy) | 🟠 | ✅ | `pkg/privacy/consent.go` `usPrivacyOptOut()` |
| GDPR / CCPA / COPPA enforcement | regulatory | Compliance | 🔴 | ✅ | 3-tier opt-out registry; contextual-downgrade on missing consent. SSP now populates `Regs`/`User.ext.consent` from the ad tag (`applyPrivacySignals`) so DSP `Evaluate` runs on real inputs (previously empty) |
| **GPP (Global Privacy Platform)** | IAB Tech Lab | Consolidated consent envelope | 🔴 | ✅ | `pkg/privacy/gpp.go` decodes the US National section (id 7) sale/share/targeted-ad opt-out → contextual via `Evaluate`. Other US state sections recognised, decode deferred (safe — downgrade only) |
| Global Privacy Control (GPC) | W3C / browsers | Browser opt-out | 🟡 | ✅ | First-class: SSP sets `Regs.ext.gpc` from `Sec-GPC`/`?gpc=1`; `Evaluate` honours it with reason `gpc` |

## 5. Identity

| Standard | Steward | Who it's for | Prominence | Our status | Where / notes |
|---|---|---|---|---|---|
| Custom identity graph | (in-house) | Cross-device linking | — | 🟨 | `pkg/identity/` — hashed email / device / IP+UA; not a standard ID |
| UID2 (Unified ID 2.0) | TTD / IAB Tech Lab op | Post-cookie addressability | 🔴 | 🟨 | Carried + resolved: `User.EIDs` (`pkg/openrtb/eid.go`), SSP populates from `?uid2`, DSP uses `UserKey`. **Identity graph now writable + resolvable** — ingest via `/v1/api/identity-links`, DSP expands UID2→linked ids for segment lookup (`dsp.identity_resolution_enabled`). Not done: token decryption via a UID2 operator + key rotation |
| Audience Taxonomy 1.1 / Data Transparency | IAB Tech Lab | Segment labelling | 🟡 | ⬜ | — |
| RampID / SharedID / ID5 / EUID | various | Publisher/resolution IDs | 🟠 | ⬜ | Mostly proprietary; deprioritise |

## 6. Creative, Measurement & Classification

| Standard | Steward | Who it's for | Prominence | Our status | Where / notes |
|---|---|---|---|---|---|
| MRC Viewability (50%/1s; video 2s) | Media Rating Council | Everyone measuring viewability | 🔴 | ✅ | `cmd/tracker/main.go` server-authoritative `IsIABViewable` |
| IAB Content Taxonomy 1.0 | IAB Tech Lab | Contextual classification | 🔴 | ✅ | Complete tier-1 set (IAB1–26) + validation/lookup/parent-resolution in `pkg/taxonomy`; classifier expands tier-2→tier-1. (Platform runs on 1.0 `IABxx` / `cattax=1`; 3.0 numeric ids would be a data migration — deferred) |
| IAB standard ad units (300×250 …) | IAB | Creative sizing | 🔴 | 🟨 | W/H supported; no validated preset list |
| MRAID 3.0 | IAB Tech Lab | In-app rich media | 🔴 | 🟨 | Signal codes only; no runtime |
| SafeFrame 2.0 | IAB Tech Lab | Creative sandboxing | 🟡 | ⬜ | — |
| GARM Brand Safety Floor | ANA (ex-WFA/GARM) | Brand safety | 🟡 | ⬜ | Framework lives on; org dissolved 2024 |

---

## Roadmap

Prioritised by **prominence × proximity × spec clarity.** Check items off as
they land and flip the status cells above.

### Phase 1 — Complete the transparency triad *(highest ROI, small specs, 2/3 done)* — ✅ SHIPPED
- [x] **SupplyChain Object (schain)** — SSP populates `Source.Ext.schain`; exchange
      validates behind `exchange.schain_enforcement` (warn default → strict). Optional
      exchange-node append behind `exchange.schain_append_node`. `pkg/openrtb/schain.go`.
- [x] **app-ads.txt** — `cmd/appadstxt/` crawler + `app_ads_txt_cache` (migration 035).
      Enforcement/warm-cache consumption remains a follow-up (parallel to ads.txt).
- [x] **Privacy-plumbing fix (pulled forward)** — SSP now stamps consent/regulatory
      signals (query params + `Sec-GPC`) onto the outbound request via
      `applyPrivacySignals`; DSP enforcement previously saw empty inputs.

### Phase 2 — Modernise privacy *(regulatory pressure rising)* — ✅ SHIPPED
- [x] **GPP** — `pkg/privacy/gpp.go` decodes the US National section (id 7) opt-out
      bits; `Evaluate` downgrades to contextual (reason `gpp_opt_out`). `Evaluate` now
      takes a `privacy.Signals` struct. Follow-up: decode other US state sections
      (8–12) + validate bit offsets against official IAB test vectors before strict use.
- [x] **GPC** — first-class `Regs.ext.gpc` set by the SSP from `Sec-GPC`/`?gpc=1`;
      `Evaluate` honours it distinctly (reason `gpc`), no longer a US-Privacy hack.

### Phase 3 — Deepen the half-built
- [x] **IAB Content Taxonomy** — `pkg/taxonomy` holds the complete tier-1 set
      (IAB1–26, spec-accurate) with validation, name lookup, and tier-2→tier-1
      resolution; `pkg/constants` expanded + the `IAB10`(Home&Garden)/`IAB21`(Real
      Estate) mix-up fixed; classifier expands subcategories to their parent.
      *(Stayed on Taxonomy 1.0 / `cattax=1` — what the platform's data uses;
      migrating to 3.0 numeric ids is a separate data migration, deferred.)*
- [x] **OpenRTB Native 1.2** execution — fully done: `pkg/native` markup, SSP native
      request + winner short-circuit, `creatives.native_assets` (mig 036) + DSP native
      match/response, and publisher-adserver `nativeHandler` renders the markup to an
      HTML card with signed impression/click trackers (`/v1/pubad/native`).
- [~] **OMID / SIMID** — server-side OMID done: SSP signals `api:[7]`, publisher-adserver
      emits `<AdVerifications>` (OM SDK verification script + signed not-executed beacon)
      in VAST behind `publisher_adserver.omid_verification_url`. *Remaining is client-side:
      the OM SDK session JS runtime + SIMID interactive creative — no OM SDK in this repo.*

### Phase 4 — Cookieless identity & advanced trust
- [~] **UID2** — request-side addressability + identity resolution done: `User.EIDs`
      + UID2 helpers (`pkg/openrtb/eid.go`), SSP populates from `?uid2`, DSP `UserKey`
      for opt-out/segments, **and the identity graph is now functional** — ingest UID2↔id
      edges via `/v1/api/identity-links` (`pkg/store/postgres` Link/Resolve), DSP expands
      a user to linked ids for the private-segment lookup (`dsp.identity_resolution_enabled`).
      *Remaining: token decryption via a UID2 operator + key rotation.*
- [x] **ads.cert 2.0** signed bid requests — `pkg/adcert` (Ed25519 sign/verify over
      canonical fields + signed timestamp for replay protection); exchange signs into
      `Source.Ext.adcert` and publishes its key at `/v1/adcert/key`; DSP verifies,
      enforces freshness (`dsp.adcert_max_age`), and fetches/refreshes the key
      (`dsp.adcert_key_url`). *Optional future: multi-key rotation with key IDs.*

### Deprioritised
OpenRTB 3.0 (2.x dominates) · VPAID (dying) · RampID (proprietary) · GARM (org dissolved).

---

## Change log
- **2026-07-06** — probabilistic matching refinements + full-flow diagram: fuzzy UA
  (strip version numbers so Chrome/120 vs /121 don't split a device;
  `identity_consumer.fuzzy_ua`), Redis-backed fingerprint buckets (pluggable
  `identityobserve.FPStore`; `identity_consumer.redis_url` → multi-replica-safe).
  Added `docs/diagrams/end-to-end-flow.md` — the current full ad-lifecycle + identity
  flow (the older `request-flow.txt` predates the standards/identity work).
- **2026-07-06** — identity auto-build made event-driven: the observer moved to
  `pkg/identityobserve`; the SSP now just publishes `adtech.identity.observed`
  (per-request signals + IP+UA fingerprint), and a new `cmd/identity-consumer`
  service subscribes and does the batching/dedup/write + probabilistic bucketing.
  Decouples the write from the serving pods and makes the probabilistic fingerprint
  state a single global view. (Run one replica; buckets are in-memory.)
- **2026-07-06** — identity auto-build gains probabilistic matching: the SSP links
  different users seen from the same IP+user-agent at a configurable confidence
  (`ssp.identity_probabilistic_*`), conservatively — exact IP+UA, and fingerprints
  seen with too many distinct ids (shared IPs) are skipped. Slots into the DSP's
  confidence gate so operators can require deterministic-only. Routed through the
  same batcher; `edgeKey` now includes source so a weak link can't block a strong one.
- **2026-07-06** — identity resolution is now transitive + confidence-gated: the
  preload adjacency carries per-edge confidence (`postgres.IdentityLink`), and the DSP
  resolver walks it breadth-first to `dsp.identity_max_depth` hops (default 3) over
  edges ≥ `dsp.identity_min_confidence` — so uid2→email→device resolve together and
  weak/probabilistic links can be excluded. Was single-hop.
- **2026-07-06** — identity graph now auto-builds: the SSP observes co-occurring
  identifiers (user_id/uid2/hashed_email/ifa) on inbound requests and writes
  deterministic edges (`identity.LinkObserved`, confidence 1.0) via an async
  batched observer (`cmd/ssp/identity.go`, off the hot path, deduped, drop-if-full;
  `ssp.identity_observe_enabled`). Graph is no longer explicit-upload-only.
- **2026-07-06** — identity preload capacity tuning: id strings are interned in the
  in-memory snapshot (each unique id one allocation, ~halves footprint), and the default
  reload interval is 5m (cuts full-rebuild churn). Comfortable to a few million ids.
- **2026-07-06** — identity resolution moved fully off the DB hot path: DSP now serves
  resolution from an in-memory snapshot of the whole graph, refreshed on an interval in
  the background (`dsp.identity_preload_interval`) — bid-path reads are lock-free map
  lookups, no Postgres per bid (warm-cache pattern, `preloadIdentityResolver`). Also
  fixed `cache.MemoryL2` to be mutex-guarded (it stands in for concurrency-safe Redis).
- **2026-07-06** — identity graph made functional (was inert): write path
  (`pkg/store/postgres` LinkIdentity/ResolveIdentity + gateway `/v1/api/identity-links`)
  and DSP read path (opt-in `dsp.identity_resolution_enabled` expands UID2/user →
  linked ids, unions private segments). Makes UID2 resolvable across devices/publishers.
- **2026-07-06** — API spec: `docs/openapi.yaml` refreshed for the standards work —
  `BidRequest` now documents source.ext.schain, source.ext.adcert(+ts), regs.ext
  (gdpr/us_privacy/gpp/gpp_sid/gpc), user.eids (UID2), and imp.video/native/tagid; new
  paths for `/sellers.json`, `/v1/adcert/key`, `/v1/pubad/native`, `/v1/pubad/video/vast`.
- **2026-07-06** — adcert key distribution: exchange publishes its Ed25519 public key at
  `/v1/adcert/key`; DSP fetches + periodically refreshes it (`dsp.adcert_key_url`,
  atomic-pointer cache, static fallback), so rotations propagate without reconfiguring
  every DSP. ads.cert now ✅.
- **2026-07-06** — adcert hardening: replay protection — a signed timestamp
  (`Source.Ext.adcert_ts`) is now part of the canonical; DSP rejects stale/future
  requests beyond `dsp.adcert_max_age` (default 5m) in addition to signature checks.
- **2026-07-06** — Phase 3 (part 4): OMID server-side — `pkg/vast` `<AdVerifications>`
  (OM SDK JavaScriptResource + verificationNotExecuted beacon), publisher-adserver emits
  it behind `publisher_adserver.omid_verification_url`; SSP already signals `api:[7]`.
  Client-side OM SDK runtime remains out of scope (no OM SDK in-repo).
- **2026-07-06** — Phase 4 (part 2): ads.cert-style signed bid requests — `pkg/adcert`
  (Ed25519 sign/verify over canonical request fields), exchange signs into
  `Source.Ext.adcert`, DSP verifies behind `dsp.adcert_enforcement` (off/warn/strict).
  Replay protection + published-key distribution remain.
- **2026-07-06** — Phase 4 (part 1): UID2 request-side addressability — `User.EIDs` +
  UID2 helpers (`pkg/openrtb/eid.go`), SSP populates from `?uid2`, DSP `UserKey`
  resolution (UID2 when cookieless) for opt-out + segment lookup. Token
  decryption/operator + identity-graph ingestion remain.
- **2026-07-06** — Phase 3 (part 3): Native 1.2 publisher-side rendering — publisher-
  adserver `nativeHandler` (`/v1/pubad/native`) turns the native response markup into an
  HTML card with signed impression/click trackers; SSP native winner short-circuit.
  **Native is now complete end-to-end.**
- **2026-07-06** — Phase 3 (part 2): OpenRTB Native 1.2 wired end-to-end through the
  auction — `creatives.native_assets` (mig 036) + model/loader, DSP native creative
  matching + native response markup, seeded native creative. Publisher-side rendering
  of the native markup is the remaining piece.
- **2026-07-06** — Phase 3 (part 1): IAB Content Taxonomy completed — new `pkg/taxonomy`
  (full tier-1 set + validation/lookup/parent-resolution), constants fixed/expanded,
  classifier wired. Native 1.2 + OMID/SIMID still open.
- **2026-07-06** — Phase 2 shipped: first-class GPC (`Regs.ext.gpc`), GPP US-National
  opt-out decode (`pkg/privacy/gpp.go`), and `Evaluate` refactored to a `privacy.Signals`
  struct. GPP/GPC rows + roadmap updated.
- **2026-07-05** — Phase 1 shipped: schain (origin at SSP + validation at exchange),
  app-ads.txt crawler + cache (migration 035), and the SSP privacy-signal plumbing fix
  (incl. GPP passthrough + GPC→USP mapping). Rows/roadmap updated above.
- **2026-07-05** — initial audit + roadmap created.
