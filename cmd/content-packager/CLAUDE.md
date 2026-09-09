# Content Packager (Job)

One-shot Job in the SSAI chain: ffmpeg-segments a source content MP4 from the
object store into a real HLS VOD origin (variant playlists + master via
`ssai.BuildMaster`) with a stamped `#EXT-X-CUE-OUT`/`CUE-IN` mid-roll break, so
`cmd/ssai` has genuine content to splice conditioned ads into. Real content is
always pre-packaged like this; we do it once into Minio.

## How it runs

- Not in the Helm chart and not a CronJob — run once, manually, after `make seed`
  lands the source media (`cmd/seed/media.go` → `media/bbb-720-10mb.mp4`). Needs
  ffmpeg on PATH: use the shared image `build/Dockerfile.transcode` (same family
  as `cmd/transcoder`), or host `go run ./cmd/content-packager` with `S3_*`
  pointing at Minio.
- Exits non-zero on any failure; reruns are safe (deterministic keys
  `{prefix}/{content_id}/...` overwrite).
- **Audio mode** (`packager.audio=true`): single `transcode.DefaultAudioProfile()`
  rendition, no master — origin is `{prefix}/{content_id}/audio/index.m3u8`
  (podcast/radio SSAI). **Container** (`packager.container`): `ts` (MPEG-TS) or
  `cmaf` (fMP4 `.m4s` + `init.mp4` — required for DASH SSAI origins).

## Config (`pkg/config/keys/contentpackager.go`, all TierStatic)

`packager.source_bucket` (adtech-creatives) / `source_key` / `content_id` /
`prefix` (ssai/content) / `break_at_segment` (2) / `break_segments` (5) /
`audio` / `container` (ts) — plus the shared `transcode.*` ladder.

## Gotchas

- `config.Setup` boots under `constants.ServiceSSAI` — env/live-config scoping
  follows the "ssai" service name, not "content-packager".
- Playlist too short for `break_at + break_segments` → `insertBreak` clamps and
  may return the playlist unchanged — the stitcher then has no avail.
- SSAI demo prerequisite: run once for the video origin
  (`ssai/content/sample/master.m3u8`) and once with `packager.audio=true`
  (docs/SSAI_CONDITIONING.md "Operational note").

## Pointers

Core: `pkg/transcode` (Runner, ParseLadder), `pkg/ssai` (ParseMedia, BuildMaster),
`pkg/store/objects/s3`. Docs: `docs/SSAI_CONDITIONING.md` (P1/R2/D2);
`docs/PLAN.md` → "Server-Side Ad Insertion (SSAI / Instream)".
