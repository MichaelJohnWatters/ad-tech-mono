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
| OpenRTB Native 1.2 | IAB Tech Lab | Native placements | 🔴 | 🟨 | `pkg/openrtb` Native struct present; no asset rendering/tracking runtime |
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
| **GPP (Global Privacy Platform)** | IAB Tech Lab | Consolidated consent envelope | 🔴 | 🟨 | Passthrough only: `gpp`/`gpp_sid` carried SSP→exchange→DSP (`RegsExt`, `cmd/ssp` `applyPrivacySignals`). Full *parsing* in `Evaluate` still Phase 2 |
| Global Privacy Control (GPC) | W3C / browsers | Browser opt-out | 🟡 | 🟨 | `Sec-GPC: 1` mapped to a US-Privacy opt-out at the SSP (`applyPrivacySignals`) |

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
| IAB Content Taxonomy 3.0 | IAB Tech Lab | Contextual classification | 🔴 | 🟨 | Only ~14 hand-picked codes in `pkg/constants`; not full taxonomy |
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

### Phase 2 — Modernise privacy *(regulatory pressure rising)*
- [~] **GPP** — `gpp`/`gpp_sid` now carried end-to-end (passthrough); still need
      full string *parsing* in `pkg/privacy/consent.go` alongside TCF/USP.
- [~] **GPC** — `Sec-GPC` mapped to a US-Privacy opt-out at the SSP; a first-class
      GPC signal in `Evaluate` (rather than the USP mapping) is the remaining work.

### Phase 3 — Deepen the half-built
- [ ] **Full IAB Content Taxonomy 3.0** — replace the 14 hand-picked codes with
      the complete list (pure data).
- [ ] **OpenRTB Native 1.2** execution — asset rendering + native event tracking.
- [ ] **OMID / SIMID** interactive-video runtime (skip VPAID — deprecated).

### Phase 4 — Cookieless identity & advanced trust
- [ ] **UID2** integration in `pkg/identity`.
- [ ] **ads.cert 2.0** signed bid requests *(low priority — thin adoption)*.

### Deprioritised
OpenRTB 3.0 (2.x dominates) · VPAID (dying) · RampID (proprietary) · GARM (org dissolved).

---

## Change log
- **2026-07-05** — Phase 1 shipped: schain (origin at SSP + validation at exchange),
  app-ads.txt crawler + cache (migration 035), and the SSP privacy-signal plumbing fix
  (incl. GPP passthrough + GPC→USP mapping). Rows/roadmap updated above.
- **2026-07-05** — initial audit + roadmap created.
