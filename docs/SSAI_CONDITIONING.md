# SSAI Ad Conditioning (Route B) — Design & Plan

Real server-side ad insertion with **runtime ad conditioning**: transcode +
segment the auction-winning ad to match the content's encoding profile, splice
real HLS segments into the content manifest, and play a seamless byte stream.
This is the production-shaped model (Google DAI / AWS MediaTailor / Yospace),
not the pre-baked demo (Route A).

Status: **built** (2026-07-08). P1–P6 implemented end-to-end; P7 partially
done (SCTE-35 DATERANGE recognition landed; CMAF/fMP4 + ID3 designed and
deferred — see P7 below). ffmpeg baked into the transcoder image. NOTE: the
ffmpeg transcode + hls.js playback path has not been exercised at runtime in
this environment (ffmpeg not installed locally; cluster flaky) — code is
unit-tested where pure, but the live splice/playback is unverified.

## Why B

A break returns an arbitrary ad from a live auction. To splice it seamlessly the
ad segments must match the content's container/codec/resolution/fps/audio and be
keyframe-aligned at segment boundaries. Route A pre-bakes fixed assets and can't
handle an arbitrary winner; Route B **conditions the winner at request time** and
caches it. That conditioning step (transcode + segment) is the whole game.

## Architecture / data flow

```
player → gateway → ssai.manifestHandler
   ├─ auction (SSP, exists) → winner: creative_id + mezzanine MP4 + duration
   ├─ transcoder.Condition(creative_id, mezzanineURL, contentProfile)
   │     ├─ cache hit? (Minio ssai/cond/{creative}/{profileHash}/index.m3u8) → return segments
   │     └─ miss → ffmpeg transcode→content profile + segment (.ts, keyframe-aligned)
   │              → write segments+index to Minio → return
   ├─ splice real ad .ts into the content variant playlist (#EXT-X-DISCONTINUITY)
   └─ server-side beacons (exists) — segment-request-driven
player fetches content .ts (Minio) + ad .ts (Minio) → one seamless stream
```

**Beacon model is unchanged and already correct:** each stitched ad segment URL
routes through `/v1/ssai/seg`, which fires the quartile beacon (segment index →
quartile) then 302s to the segment. We only swap the redirect target from "a
whole MP4" to "the real conditioned `.ts`." So the stitch + beacon logic carries;
the net-new piece is conditioning.

## Components

| Component | Role |
|---|---|
| `pkg/transcode` | ffmpeg arg builder + HLS packaging/parse + the **Profile** model + profileHash. Pure/logic-testable (no ffmpeg exec in unit tests). |
| `pkg/hls` (or fold into pkg/ssai) | master + variant playlist build for ABR; discontinuity/timing helpers. |
| `cmd/content-packager` (Job) | one-time: ffmpeg-segment a sample content clip → real HLS (`.ts` + variant/master manifests) in Minio. Real content is always pre-packaged. |
| `cmd/transcoder` (service, :809x) | conditioning API: `Condition(creative, mediaURL, profile)` → transcode+segment→Minio, **cached** by `(creative, profileHash)`. ffmpeg in image. |
| `cmd/ssai` (extend) | call transcoder, reference real conditioned segments, correct discontinuity/timing, ABR variant selection. |
| SSAI tab player | swap to **hls.js** (OSS, self-hostable, no Google) to play the real stitched HLS. |

## Storage layout (Minio, creatives bucket or a dedicated `ssai` bucket)

```
ssai/content/{contentId}/master.m3u8
ssai/content/{contentId}/{rung}/index.m3u8 + seg_%d.ts        # per ABR rung
ssai/cond/{creativeId}/{profileHash}/index.m3u8 + seg_%d.ts    # conditioned ad, per profile
```

Cache = existence of `ssai/cond/{creative}/{profileHash}/index.m3u8`. Optional
Postgres/Redis index for TTL/eviction + metrics; Minio existence check is the
source of truth.

## Encoding Profile model

```
Profile{ Container(ts|cmaf), VCodec(h264|hevc), Width, Height, FPS,
         VBitrateKbps, ACodec(aac), ASampleRate, ABitrateKbps, SegDurSec }
ProfileHash = short hash of the normalized Profile → cache key + Minio path.
```

Content defines the profile(s) — one per ABR rung. The ad is conditioned once per
distinct profile it's asked for.

## ffmpeg commands (constructed in pkg/transcode; run in-container)

Content packaging (per rung):
```
ffmpeg -i in.mp4 -c:v libx264 -profile:v main -b:v {V}k -maxrate {V}k -bufsize {2V}k \
  -vf scale={W}:{H} -r {FPS} -force_key_frames "expr:gte(t,n_forced*{SEG})" \
  -c:a aac -ar {AR} -b:a {A}k -f hls -hls_time {SEG} -hls_playlist_type vod \
  -hls_segment_type mpegts -hls_segment_filename seg_%d.ts index.m3u8
```
Ad conditioning: identical flags for the target Profile (that's the whole point —
byte-compatible with content), against the ad mezzanine.

## Conditioner API (cmd/transcoder)

```
POST /v1/transcode/condition
  { creative_id, media_url, profile{...} }
→ { profile_hash, segments:[{uri,duration}], index_url, cached:bool }
```
- Cache-first: HEAD `ssai/cond/{creative}/{profileHash}/index.m3u8` → return if present.
- Miss: fetch mezzanine → ffmpeg to a temp dir → upload segments+index to Minio →
  return. Single-flight per (creative, profileHash) so concurrent breaks don't
  double-transcode.

## Latency handling (the core risk)

On-the-fly transcode is **seconds**; a break can't stall. Mitigations, layered:
1. **Cache** — instant after first conditioning of an ad+profile.
2. **Slate-on-miss** — if not cached, return a slate/house ad for THIS break and
   condition async; the ad is ready for the next viewer.
3. **Pre-warm** (P6) — a job conditions active creatives against the content
   profiles ahead of time (cron), so the cache is warm before serving.

## Phases

- **P1 — Content packaging** (M): `pkg/transcode` Profile + ffmpeg arg builder +
  HLS build/parse; `cmd/content-packager` Job segments a sample clip → Minio;
  SSAI stitches this real origin. Unit tests on arg building + HLS logic.
- **P2 — Transcoder service, single profile, on-the-fly + cache** (L):
  `cmd/transcoder` + ffmpeg-in-image + Minio cache + single-flight. The core.
- **P3 — Stitcher integration** (M): SSAI calls the transcoder, splices real ad
  `.ts`, correct `#EXT-X-DISCONTINUITY`/`DISCONTINUITY-SEQUENCE`/timing; beacons
  segment-driven against real segments; **slate-on-miss**.
- **P4 — hls.js player** (M): SSAI tab plays the seamless stitched stream.
  **← MVP: true byte-concatenated SSAI that plays, single profile.**
- **P5 — ABR** (L): multi-rung content + a conditioned ad variant per rung +
  master playlist; player adapts.
- **P6 — Pre-warm + ops** (M): pre-condition cron, cache TTL/eviction, metrics
  (conditioning latency, cache-hit rate, transcode failures), slate accounting.
- **P7 — Stretch** (XL): real SCTE-35 parsing (live), CMAF/fMP4 (HLS+DASH), DRM
  hooks, ID3 timed-metadata beacons.
  - **SCTE-35 DATERANGE — DONE.** `ssai.ParseMedia` now recognises broadcast-
    native ad signalling: an `#EXT-X-DATERANGE` carrying `SCTE35-OUT` (or
    `CUE="OUT"`) opens a break of `PLANNED-DURATION` seconds and maps to our
    existing CUE-OUT model, so the stitcher treats SCTE-35 content identically to
    CUE-OUT/CUE-IN content. A matching `SCTE35-IN` DATERANGE closes it; with no
    explicit close (pure SCTE-35), `closeScteBreaks` auto-closes at the segment
    where cumulative content duration first reaches the planned length. Tested
    in `pkg/ssai/manifest_test.go:TestParseSCTE35Daterange`. (Full binary SCTE-35
    splice_info_section decoding from a live transport stream is still future;
    the manifest-level DATERANGE form is what HLS packagers emit and is enough
    to drive insertion.)
  - **CMAF/fMP4 — designed, deferred.** `Profile.Container` already accepts
    `cmaf`/`fmp4` and `FFmpegArgs` emits fMP4 segments; the remaining work is the
    init segment: ffmpeg writes `init.mp4` + `#EXT-X-MAP:URI="init.mp4"`, so
    `runner.readHLSOutput` must capture the init object, the conditioner must
    upload it under the cond prefix, and `ssai.ParseMedia`/`Render` must preserve
    `#EXT-X-MAP`. The seamless-splice constraint is stricter for fMP4 (the ad's
    init must be codec-compatible with content's), which is exactly the Profile
    match we already enforce. DASH is a second manifest flavour over the same
    conditioned segments.
  - **ID3 timed-metadata beacons — designed, deferred.** An alternative to
    segment-driven server beacons: embed beacon triggers as ID3 `PRIV`/`TXXX`
    frames in the ad segments (`ffmpeg -metadata` / a muxing pass), and the
    player fires them on the `hls.js` `FRAG_PARSING_METADATA` (or CTV native ID3)
    event. Useful where the player must attribute quartiles client-side; our
    server-authoritative segment beacons already cover the common case.

## Post-review enhancements (2026-07-09)

A review of P1–P7 turned up three data-accuracy defects (fixed) and a set of
feature gaps (added):

- **Quartile beacons** now fire on VAST time-mark *crossings* (a segment may
  cross several), so all of start/firstQuartile/midpoint/thirdQuartile/complete
  fire exactly once regardless of how the ad segments — the old one-event-per-
  segment logic dropped thirdQuartile and double-fired start.
- **Impressions** fire on first-ad-segment *fetch*, not at manifest generation,
  so an abandoned mid/post-roll never books a phantom impression.
- **Cold-miss** keeps content + warms the conditioner instead of splicing the
  raw mezzanine as pseudo-segments (which stalled the player).
- **Ad pods**: `fillBreak` runs back-to-back auctions per avail until the break
  is full or `ssai.max_pod_ads` (default 4), each ad conditioned + beaconed
  independently with a discontinuity at its boundary. `runAuction` advertises
  the remaining slot via `max_duration`.
- **Slate**: `ssai.slate_creative_id` splices a conditioned house clip (no
  beacons) into an unfilled avail instead of dropping to content.
- **VAST error beacon**: a winner that can't be conditioned in time fires an
  `error` video event to the tracker before falling through to slate/content.
- **Stitcher metrics** on `/metrics`: `ssai_breaks_total{filled|slate|unfilled|
  error}`, `ssai_condition_cache_total{hit|miss}`, `ssai_ad_seconds_total`,
  `ssai_pod_ads`.
- **Audio SSAI**: `Profile.AudioOnly` + `DefaultAudioProfile()` drop the video
  pipeline (`-vn`, no scale/fps/keyframes) so audio ads condition to AAC-only
  HLS; `cmd/prewarm` selects the audio profile for `format='audio'` creatives.

Still open (designed, not built): a proper audio *stitcher* serving path (the
conditioning is done, but cmd/ssai is video-only), CMAF init-segment handling,
ID3 beacons, and cache-bust on creative replacement.

## Risks

- **ffmpeg CPU** — transcoding is heavy; transcoder needs generous limits + HPA;
  keep it off the hot serving path (async + cache).
- **Profile mismatch** — any codec/fps/audio drift breaks the seamless splice;
  strict Profile equality + a golden test matrix.
- **First-request latency** — handled by slate-on-miss + pre-warm.
- **CTV player quirks** — discontinuity handling varies; test hls.js + Safari
  native first; real CTV devices later.

## Testing

- `pkg/transcode`: unit-test ffmpeg arg construction + HLS parse/build + profile
  hashing (no ffmpeg exec).
- `cmd/transcoder`: integration test with a tiny bundled fixture MP4 → condition
  → assert segments + index produced + cached (requires ffmpeg; runs in the
  container / CI image, `-tags=ffmpeg`).
- e2e: full stitch + browser play (manual, SSAI tab).

## Config keys (new)

```
transcoder.port, transcoder.bucket, transcoder.prefix (ssai/cond),
transcoder.ffmpeg_path (default "ffmpeg"), transcoder.timeout,
ssai.transcoder_url, ssai.content_id, ssai.content_profile (default rung),
ssai.slate_media_url
```
Plus a `GATEWAY_TRANSCODER_URL` if we ever proxy it; the transcoder is internal
(ssai → transcoder), so no gateway proxy needed.
```
```
