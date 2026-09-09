# Prewarm (Job)

One-shot Job in the video/audio SSAI chain: conditions every active video/audio
creative against the transcoder BEFORE first serve, so the stitcher's ad-break
lookup is a cache hit (no cold-transcode slate). Complements the stitcher's
slate-on-miss + async warm — this is layer 3 of the SSAI latency mitigation
(docs/SSAI_CONDITIONING.md → "Latency handling", P6).

## How it runs

- Not in the Helm chart and not a CronJob — run manually (`go run ./cmd/prewarm`
  with `DATABASE_URL` + reachable transcoder), per cmd/transcoder/CLAUDE.md.
  The P6 design intent is periodic; wire a CronJob if that ever matters.
- Rerun-safe: the transcoder caches by creative+contentVersion+profile hash, so
  already-conditioned combos are cheap hits.
- Per-creative condition failures are logged at Warn and counted — the job
  continues and still exits 0. Only DB open/query errors exit non-zero. Read the
  final `prewarm complete` log line (`warmed`/`failed`) to know if it worked.

## What it does (main.go)

1. Selects `creatives` rows with `format IN ('video','audio')` and a non-empty
   `asset_url` (no `active` filter despite the doc comment — every media
   creative is warmed).
2. Video → one condition call per ABR rung from the shared `transcode.ladder`
   key (`transcode.ParseLadder`); audio → single `transcode.DefaultAudioProfile()`.
3. `POST {prewarm.transcoder_url}` + `routes.TranscodeCondition`
   (`/v1/transcode/condition`) per creative×profile; 6-minute client timeout each.

## Config keys (`pkg/config/keys/prewarm.go`)

`prewarm.transcoder_url` (TierStatic, default `routes.DefaultTranscoderURL`,
transcoder :8094) — plus the shared `transcode.*` entries (ladder, TierLive).

## Gotchas

- **RLS:** `creatives` is RLS-protected and the query is a bare-pool read with
  no tenant GUC — under `adtech_app` it silently sees 0 rows and warms NOTHING.
  Run with a DB role granted cross-tenant read / BYPASSRLS (the in-code comment
  says so; this is the standard bare-pool-on-RLS-table trap).
- `config.Setup` boots under `constants.ServiceTranscoder` ("transcoder"), not a
  "prewarm" service name — env/live-config scoping follows the transcoder
  (same pattern as content-packager booting as "ssai").
- Changing `transcode.ladder` changes the profile set — re-run the
  content-packager too so origins match (see the ladder key's own doc string).

## Pointers

- `docs/SSAI_CONDITIONING.md` — P6 (pre-warm + ops), "Latency handling"
- `docs/PLAN.md` → "Video Ads, SSAI, and CTV", "Server-Side Ad Insertion (SSAI / Instream)"
- `cmd/transcoder/CLAUDE.md` — the service this job drives
