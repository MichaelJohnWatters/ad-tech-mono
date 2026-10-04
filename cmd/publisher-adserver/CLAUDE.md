# Publisher Ad Server

The publisher-side ("GAM-shaped") serving layer (:8088). Sits in front of `cmd/ssp` and arbitrates direct-sold publisher line items against the programmatic auction, which becomes the fall-through demand source.

## Responsibilities

- Per-impression arbitration ladder (`pkg/publisheradserver/arbitration`): sponsorship > guaranteed-behind-pace > programmatic fall-through > publisher house line item > platform house ad (`stub_on_nobid`) > honest no-fill. (`preferred` is a modelled tier but NOT implemented in the `Decide` ladder yet — it falls through; see `arbitration.go`.)
- Delivery pacing for guaranteed line items (`pkg/publisheradserver/pacing`): hit the committed target by end-of-flight — Redis actuals counter shared across pods, fail-open
- Programmatic fall-through fans out in parallel to our SSP AND every external Prebid Server in `publisher_adserver.prebid_servers` (live key, CSV); highest non-nobid price wins
- Direct wins render via the ad server (`POST /v1/ad/serve`, `routes.AdServe`) with `CampaignID` = the publisher line item ID (stable freq-cap/attribution key)
- External Prebid wins get wrapped with OUR beacons (impression pixel + inline IAB viewability observer + click_url) so reporting keeps zero slippage without touching the bidder's adm or click chain
- Format handlers: VAST 4.2 video (with optional OMID `<AdVerifications>`), VMAP schedule (breaks point back at the VAST endpoint via `publisher_adserver.public_url`), native, audio

## Interfaces

HTTP (all in `pkg/routes`):
- `GET /v1/pubad/serve` - display JSON (`source`: direct | prebid | house | none)
- `GET /v1/pubad/video/vast`, `/v1/pubad/video/vmap`, `/v1/pubad/native`, `/v1/pubad/audio`
- `/debug/pubad/line-items` + `/debug/cache/refresh` (only when `debug.endpoints_enabled`)
- Gateway proxies under `routes.ProxyPubAd` (`/v1/api/pubad/`)

NATS published (nil-tolerant — NATS down skips publishing, never fails the serve):
- `adtech.direct.win` (DirectWinEvent — direct-sold serves leave a reporting record)
- `adtech.prebid.outbound.win` (PrebidOutboundWinEvent)
- `adtech.serve.nofill` (fill-rate signal when every source no-bids)

NATS consumed: warm-cache invalidates only (`SubjectCacheInvalidatePublisherLineItems`, `SubjectCacheInvalidatePlacements`, `SubjectCacheInvalidateHouseAds`).

## Key Packages Used

- `pkg/publisheradserver/` - line item model + `arbitration/` + `pacing/` + `prebidclient/`
- `pkg/cache/warm/` - three warm caches: publisher line items, placements, house ads (poll + NATS invalidate, `RetryingLoader` self-heals Postgres)
- `pkg/adserving/` - macro-built tracker URLs; VAST/audio click URLs signed with the ACTIVE `hmac_tracker` secret (`secrets.WatchActive`, follows rotation live)
- `pkg/houseads/` - deterministic weighted house-ad pick (trace-derived FNV seed — time/rand banned)

## Dependencies

- SSP (`publisher_adserver.ssp_url`), Ad Server (`.adserver_url`), Tracker (`.tracker_url`) — all HTTP
- Postgres (warm-cache loaders only), Redis (pacing actuals, self-healing L2), NATS (events + invalidates; poll-only if unreachable)

## CRITICAL

- **Multi-replica safe** (helm: 3 replicas, HPA 2–5): request-scoped serving, pacing state in shared Redis. See `docs/MULTIPOD.md`.
- **`publisher_adserver.tracker_url` MUST be browser-reachable** — cluster DNS like `tracker:8083` never resolves from end-user browsers; in pod mode point at the gateway's `/v1/t/*` proxy (helm sets `http://localhost:8080`).
- **Readiness gates on the placement + line-item caches only.** The house-ad cache never blocks readiness — an empty house-ad set is a valid state (honest no-fill).
- **`stub_on_nobid` is OFF by default** (real-data-only rule): a no-bid returns a structured 200 `no_fill` body, never fake content. HTTP errors mean infrastructure; `no_fill: true` means the auction ran.
- **Boot-retry doctrine**: loaders lazy-open/reconnect Postgres, Redis is `SelfHealingL2`, the event publisher is nil-tolerant — nothing latches a dead fallback.
- Forward the visitor's full query to the SSP (`forwardSSPQuery`) — collapsing geo/device/consent to hardcoded defaults makes every targeted campaign no-bid ("no thin requests"). Dev fallbacks (USA / UA-derived device) apply only when the caller sent nothing.

## Architecture Details

See `docs/PLAN.md` -> "Publisher-Side Ad Server (\"GAM-shaped\" features)", "Prebid Server Integration", "VAST (Video Ad Serving Template)", "VMAP (Video Multiple Ad Playlist)"

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New serve endpoint or demand source?** Update `docs/PLAN.md` -> Publisher-Side Ad Server + the C4 model (`docs/diagrams/workspace.dsl`, `make c4`)
- **New NATS subject?** Update `docs/PLAN.md` -> NATS Subjects table + NATS Event Flow diagram
- **Changed arbitration ladder or fall-through order?** Update the serving-flow diagram (see `docs/diagrams/README.md` "Update when" column)
- **New dependency?** update the C4 model (`docs/diagrams/workspace.dsl`) and run `make c4`
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
