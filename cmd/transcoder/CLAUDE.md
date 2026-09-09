# Transcoder Service

Runtime ad-conditioning for SSAI (:8094). Given an auction-winning video/audio ad + the
content's encoding profile, it ffmpeg-transcodes and segments the ad into HLS that is
byte-compatible with the content stream, caching the result in the object store so each
(creative, media-version, profile) is conditioned exactly once. The SSAI stitcher
(`cmd/ssai`) calls it per ad break.

## Responsibilities

- Condition an ad to a `transcode.Profile` (codec/resolution/bitrate/segmenting), cache-first
- Serve `cache_only=1` lookups for the stitcher's serving path — never transcodes inline there
- Cache conditioned segments in S3 under `{transcoder.prefix}/{creative}-{mediaURLHash}/{profileHash}/`
- Emit conditioning metrics (`adtech_transcoder_conditions_total` result=cached|conditioned|failed, `adtech_transcoder_condition_duration_seconds`)

## Interfaces

- `POST /v1/transcode/condition` (`routes.TranscodeCondition`) — body `{creative_id, media_url, profile}`; returns segment list + durations (+ fMP4 init URI). Slow on a cold miss (ffmpeg, seconds), instant on a hit.
  - `?cache_only=1` — return the conditioned ad if already cached, else 404. The stitcher uses this so a manifest response never blocks on ffmpeg; on a miss it keeps content/slate and warms async.
- No gRPC. No NATS — this service neither publishes nor consumes events.

## Callers

- `cmd/ssai` — per-break lookup via `ssai.transcoder_url` (helm: `http://transcoder:8094`; empty disables conditioning)
- `cmd/prewarm` — periodic job that conditions every active video/audio creative across the ABR ladder BEFORE first serve (`prewarm.transcoder_url`). Not wired as a chart CronJob — run manually.

## Key Packages Used

- `pkg/transcode/` — `Conditioner` (cache-first + single-flight), `Runner` (ffmpeg exec), `Profile`/ladder
- `pkg/store/objects/s3/` — segment cache (Minio locally, real S3 elsewhere)
- `pkg/config/keys/transcoder.go` — `transcoder.port|bucket|prefix|public_base|ffmpeg_timeout` (+ Raw `transcoder.url`)

## Dependencies

- ffmpeg in the image (`build/Dockerfile.transcode`) — `/readyz` fails if `exec.LookPath` can't find it
- S3/Minio (bucket `transcoder.bucket`, default `adtech-creatives` — public-read, same as creatives; segment URLs are built from `transcoder.public_base`, the gateway `/v1/creatives` proxy)
- No Postgres, Redis, or NATS

## CRITICAL: Invariants & Gotchas

- **S3 endpoint must be scheme-less** — main.go strips `http(s)://` before `objs3.New` (the scheme'd-endpoint latch once silently broke media services' object stores post-Helm). Keep `S3_ENDPOINT` as `host:port`.
- **Store-init failure is non-fatal at boot** — it logs ERROR and keeps serving (readiness only checks ffmpeg); conditioning then fails per-request. Don't "fix" this into a silent fallback (boot-latch doctrine).
- **Cache key includes a hash of `media_url`** — replacing a creative's asset URL busts the cache; overwriting the SAME url in place is NOT detected. Version asset URLs to force a re-condition.
- **Single-flight is per-pod** — concurrent breaks for one (ad, media, profile) collapse to one transcode within a pod. Helm runs 1 replica (chart default; no `replicas`/`hpa` set); extra replicas share the S3 cache but can duplicate cold transcodes.
- Serving paths must use `cache_only=1`; a cold `Condition` can take seconds (server `WriteTimeout` is 5m, `transcoder.ffmpeg_timeout` default 3m, TierLive).
- Mezzanine fetch: our own `/v1/creatives/...` URLs are read straight from the object store (in-cluster pods can't reach the browser-facing gateway host); anything else is HTTP-GET.

## Architecture Details

See `docs/PLAN.md` -> "Video Ads, SSAI, and CTV" (esp. "Service 2: Video Transcoder", "Creative Conditioning (Color Space, Frame Rate, Codec Profile)") and `docs/SSAI_CONDITIONING.md`.

## Diagram Updates

If you change this service, check if diagrams need updating:
- **New endpoint or caller?** Update `docs/PLAN.md` -> Video Services Architecture
- **New NATS subject (currently none)?** Update `docs/PLAN.md` -> NATS Subjects table + NATS Event Flow diagram
- **New dependency?** Update `docs/diagrams/architecture.d2` and run `make diagrams`
