# Adtech Standards — Coverage & Roadmap

**Status:** living document · **Last audited:** 2026-07-05

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
| ads.cert 2.0 (signed bids) | IAB Tech Lab | Exchange/DSP anti-spoof | 🟡 | ⬜ | Low real-world adoption |

## 3. Video / Audio / CTV

| Standard | Steward | Who it's for | Prominence | Our status | Where / notes |
|---|---|---|---|---|---|
| VAST 4.2 | IAB Tech Lab | Video ad serving | 🔴 | ✅ | `pkg/vast/vast.go`, `cmd/publisher-adserver/vast.go`, full tracking events |
| VMAP 1.0 | IAB Tech Lab | Ad break scheduling | 🔴 | ✅ | `cmd/publisher-adserver/vmap.go` — pre/mid/post-roll, independent auctions |
| OMID / Open Measurement | IAB Tech Lab | Viewability/verification | 🔴 | 🟨 | API code `7=OMID` recognised; no runtime |
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
| UID2 (Unified ID 2.0) | TTD / IAB Tech Lab op | Post-cookie addressability | 🔴 | ⬜ | Leading open cookieless ID |
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
- [ ] **OMID / SIMID** interactive-video runtime (skip VPAID — deprecated).

### Phase 4 — Cookieless identity & advanced trust
- [ ] **UID2** integration in `pkg/identity`.
- [ ] **ads.cert 2.0** signed bid requests *(low priority — thin adoption)*.

### Deprioritised
OpenRTB 3.0 (2.x dominates) · VPAID (dying) · RampID (proprietary) · GARM (org dissolved).

---

## Change log
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
