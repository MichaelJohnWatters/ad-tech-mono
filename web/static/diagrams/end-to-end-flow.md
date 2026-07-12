# End-to-End Flow — full ad lifecycle + standards overlays

**Current as of 2026-07-06.** The single "how everything flows" reference:
one ad request from the user's browser all the way through auction, serve,
tracking, billing — annotated with the transparency/trust, privacy, format, and
identity standards layered on top. Companion to `architecture.d2` (static
topology) and `request-flow.txt` (ASCII entry-point map).

Legend for the standards annotations: 🔗 supply-chain/trust · 🔒 privacy ·
🎯 identity · 🎬 format.

## 1. Ad request → auction → serve → track

```mermaid
sequenceDiagram
    autonumber
    participant B as Browser / Pub SDK
    participant PA as publisher-adserver
    participant SSP as SSP
    participant EX as Exchange
    participant DSP as DSP
    participant AS as Ad Server
    participant TR as Tracker
    participant N as NATS + Reporting

    B->>PA: GET /v1/pubad/serve|native|video/vast?placement_id
    PA->>SSP: GET /v1/ssp/serve?channel=…&uid2=…&gdpr=…&gpp=…
    Note over SSP: build OpenRTB 2.6 bid request +<br/>🔗 schain (Source.ext.schain, SSP=seller of record)<br/>🔒 Regs (gdpr/us_privacy/gpp/gpc) + User consent<br/>🎯 User.EIDs (UID2) + audience segments<br/>🎬 imp = banner / video(api:OMID) / native 1.2
    SSP-->>N: publish adtech.identity.observed 🎯 (see §2)
    SSP->>EX: POST /v1/openrtb/auction
    Note over EX: ads.txt gate · 🔗 schain validate ·<br/>🔗 ads.cert SIGN (Source.ext.adcert + ts)
    EX->>DSP: POST /v1/openrtb/bid (fan-out, smart-routed)
    Note over DSP: 🔗 ads.cert verify + freshness ·<br/>🔒 privacy.Evaluate (consent gate) ·<br/>🎯 resolve UID2→linked ids→segments ·<br/>targeting + pacing + budget + shade
    DSP-->>EX: bid (AdM: banner HTML / MediaURL / native markup)
    Note over EX: auction (deal priority: PG>Pref>PMP>Open)
    EX-->>N: adtech.auction.win 🔒 (single source of truth for cost)
    EX-->>SSP: BidResponse (winner)
    alt display
        SSP->>AS: POST /v1/ad/serve → rendered HTML + signed trackers
    else video
        PA->>PA: build VAST 4.2 + 🎬 OMID <AdVerifications> + signed trackers
    else native 🎬
        PA->>PA: render native card + signed impression/click trackers
    end
    SSP-->>PA: HTML / VAST / native fragment
    PA-->>B: ad markup
    B->>TR: GET /v1/t/imp|click|view (HMAC-signed URLs)
    Note over TR: verify sig · server-authoritative IAB viewability
    TR-->>N: adtech.events.impression|click|view
    Note over N: Reporting: analytics + billing accrual<br/>(reserve/settle per bid model)
```

## 2. Identity subsystem (auto-build → resolve)

Identity is decoupled: many services *observe* cheaply and publish; one consumer
*writes*; the DSP *resolves* from an in-memory snapshot on the hot path.

```mermaid
sequenceDiagram
    autonumber
    participant SSP as SSP (ad-tag reqs)
    participant EX as Exchange (inbound Prebid)
    participant N as NATS
    participant IC as identity-consumer
    participant PG as Postgres (identity_graph)
    participant DSP as DSP

    Note over SSP,EX: see identifiers co-occur on a request<br/>(user_id, uid2, hashed_email, ifa, IP+UA)
    SSP-->>N: adtech.identity.observed {ids, fingerprint}
    EX-->>N: adtech.identity.observed {ids, fingerprint}
    N->>IC: deliver observation
    Note over IC: batch + dedup ·<br/>deterministic edges (conf 1.0) ·<br/>probabilistic IP+UA (conf <1.0, shared-IP capped,<br/>fuzzy UA opt-in, buckets in mem or Redis)
    IC->>PG: LinkIdentity(edges)  [batched]
    loop every identity_preload_interval (5m)
        DSP->>PG: LoadIdentityGraph → in-memory snapshot (interned)
    end
    Note over DSP: on each bid: ResolveIdentity(userKey)<br/>= transitive BFS (max_depth) over edges<br/>≥ min_confidence — lock-free, zero DB
    DSP->>DSP: expand user → linked ids → union private segments
```

## What each standard adds (pointer)

| Layer | Standard | Where |
|---|---|---|
| 🔗 Trust | ads.txt · app-ads.txt · sellers.json · schain · **ads.cert** (sign+ts+key dist) | `pkg/fraud`, `cmd/adstxt`/`appadstxt`, `cmd/gateway/sellers.go`, `pkg/openrtb/schain.go`, `pkg/adcert` |
| 🔒 Privacy | TCF · USP · GDPR/CCPA/COPPA · GPC · GPP | `pkg/privacy` (`Signals`, `gpp.go`), SSP `applyPrivacySignals`, DSP `Evaluate` |
| 🎯 Identity | UID2 (`User.EIDs`) · identity graph (observe→consume→resolve) | `pkg/openrtb/eid.go`, `pkg/identityobserve`, `cmd/identity-consumer`, DSP `cmd/dsp/identity.go` |
| 🎬 Formats | OpenRTB Native 1.2 · VAST 4.2 + OMID | `pkg/native`, `pkg/vast`, `cmd/publisher-adserver` |
| Classification | IAB Content Taxonomy 1.0 | `pkg/taxonomy` |

See `docs/STANDARDS.md` for per-standard status and the change log.

