# SSAI Stitcher Service

Server-side ad insertion (:8093). Sits between the video/audio player and the origin content: the player fetches the HLS/DASH manifest from here, and the stitcher splices auction-won ads directly into the stream at each `#EXT-X-CUE-OUT`/`CUE-IN` break — ad blockers can't strip them. All ad beacons fire server-side; the player never knows it's playing ads.

## Responsibilities

- Fetch (or serve the built-in sample) origin manifest; per break run a real SSP auction (`GET {ssp}/v1/ssp/serve?channel=video|audio`), back-to-back up to `ssai.max_pod_ads` per avail (ad pods)
- Splice the winner's pre-conditioned segments (from the transcoder cache) over the break; slate (`ssai.slate_creative_id`) or keep content when unfilled
- Fire HMAC-signed impression + VAST quartile beacons server-side (built via `pkg/adserving` MacroContext — identical URLs to the client-side flow, passes tracker signature validation)
- ABR: rewrite HLS master variants/renditions to per-rung stitcher URLs; DASH single-rung MPD (multi-rung behind `ssai.dash_multi_rung`); optional timed metadata + OMID (`ssai.timed_metadata`, `ssai.omid_*`)
- RECORD the frequency cap against the ad server when an ad is actually stitched

## Interfaces

HTTP (see `pkg/routes/routes.go`, "SSAI Stitcher"):
- `GET /v1/ssai/manifest.m3u8` / `manifest.mpd` - player-facing stitch endpoint (`?origin=`, `?channel=audio`, `?rung=`, `?format=mpd`)
- `GET /v1/ssai/seg` - per-ad-segment beacon endpoint: fires the pre-signed `beacon` URLs server-side, 302-redirects to the real media
- `GET /v1/ssai/content.m3u8` - built-in sample origin (video + audio variants) for the demo
- `/healthz`, `/readyz`, `/metrics` (custom `adtech_ssai_*` counters/histogram: break fill outcomes, conditioning cache hit/miss, pod depth, ad seconds — `metrics.go`)

NATS: none for business events — only the config live-layer invalidate wired by `config.Setup`. Browsers reach it via the gateway proxy (`/v1/api/ssai/`, `gateway.ssai_url`).

## Key Packages Used

- `pkg/ssai/` - manifest parse/stitch (master, media, breaks, segments)
- `pkg/dash/` - MPD assembly (multi-period VOD, quartile EventStreams, OMID)
- `pkg/transcode/` - ladder/profiles; talks to the transcoder (:8094) for conditioned segments
- `pkg/adserving/` - signed beacon URL construction (MacroContext)
- `pkg/store/objects/s3/` - reads `/v1/creatives/...` origins straight from the bucket

## Dependencies

- SSP (`ssai.ssp_url`, HTTP `/v1/ssp/serve`) - per-break auctions
- Transcoder (`ssai.transcoder_url`) - cache-only conditioned-segment lookups + async warms
- Ad server (`ssai.adserver_url`, POST `/v1/ad/serve` CapModeRecord) - freq-cap RECORD on stitch
- Tracker (`ssai.tracker_url`) - server-side beacons
- Minio/S3 (`ssai.creatives_bucket`) - in-cluster origin reads (the stitcher can't reach the browser-facing gateway host); optional, HTTP-fetch fallback

Helm: 3 replicas + HPA (2-5) — stateless, safe to scale. `ssai.public_url` must be the BROWSER-reachable origin (the stitched manifest points segment URLs back here).

## CRITICAL Invariants

- **Freq-cap PEEK/RECORD split.** The auction call sets `cap_defer=1` so the SSP only PEEKs; `recordFreqCap` RECORDs (user + SSP-derived household id) only when an ad is actually stitched. Impressions count stitches, not serve decisions — without this, conditioning misses/no-bids/re-polls burn cap slots. Cap record is best-effort, 2s timeout, never breaks serving.
- **One fresh trace per pod ad.** `fillBreak` mints a distinct 32-hex trace per pod auction and forces it as the outbound `traceparent` (deliberately NOT `tracing.InjectHTTP`). Sharing the manifest trace makes the tracker's `("impression", trace)` dedup drop all but the first pod ad.
- **Never block serving on ffmpeg.** Serve-time conditioning is `cache_only=1`; a miss fires a VAST error beacon, warm-conditions async (detached ctx, 6-min client), and slates/keeps content. Cold conditioning is minutes.
- **Beacons must be the pre-signed URLs built at stitch time**, fired with a browser-shaped UA — never reconstruct/hand-roll a sig (fails HMAC validation) or use a bot UA (fraud check drops it). They are SEALED (with the redirect target) into the opaque `/v1/ssai/seg?t=` token (`segtoken.go`, AES-GCM keyed off `adserving.ActiveSigningKey()` → consistent across replicas), NOT plaintext params — so the client-facing URL never exposes price/advertiser/sig and gives ad blockers no handle. The handler opens the token server-side to fire + 302. Only opaque ops ids (`session/ad/break/seg`) stay plaintext.
- S3 endpoint config may carry a scheme; `main.go` strips `http(s)://` before `objs3.New`. Store-init failure degrades to HTTP-fetched origins with a Warn — don't let it latch silently if you touch this.
- Content-segment URIs must be resolved absolute against the origin URL (the stitched manifest is re-served from a different host).

## Architecture Details

See `docs/PLAN.md` -> "Video Ads, SSAI, and CTV", "Server-Side Ad Insertion (SSAI / Instream)", "SSAI Failover and Slate Content", "Video Services Architecture", "NATS Subjects for Video".

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New SSAI endpoint or beacon path?** Update `docs/PLAN.md` -> Video Services Architecture + the relevant flow diagram in `docs/diagrams/` (see its README "Update when" column)
- **New dependency (e.g. a new upstream for auctions/conditioning)?** update the C4 model (`docs/diagrams/workspace.dsl`) and run `make c4`
- **Changed beacon/quartile semantics?** Update `docs/PLAN.md` -> Server-Side Ad Insertion section
- **C4 model:** update this service's `component` block + `component <id>` view in `docs/diagrams/workspace.dsl` if you add/remove/rename a component or change a dependency. Keep ids service-prefixed and the DSL valid.
