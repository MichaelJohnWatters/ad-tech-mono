# SSAI review remediation plan

Self-contained work plan from an adversarial correctness review of the SSAI
epic (the whole Route B ad-conditioning stack + DASH + timed metadata + OMID,
merged to `main`). Two findings were independently verified against the code;
the rest are review findings with the confidence noted. Execute on branch
`fix/ssai-review-fixes` (this doc is its first commit).

Status: **planned 2026-07-10, not started.** Tick items as they land.

## How to verify fixes (READ FIRST — the e2e environment has gotchas)
- Unit tests: `go test ./...` (67 pkgs green as of the merge).
- SSAI live smoke: `make ssai-smoke` (needs `tilt up` + ffmpeg; installed locally).
- e2e: `make test-e2e` — it now runs `scripts/e2e-preflight.sh` which pins
  reporting to the `memory` analytics backend (ADR 0001; the harness verifies via
  in-memory `/debug` read-back which 501s on ClickHouse).
- **If e2e wipes 0/all-fail after an OrbStack idle-suspend:** NATS JetStream PVC
  corruption. Fix = `kubectl delete statefulset nats -n adtech && kubectl delete
  pvc -l app=nats -n adtech`, let Tilt recreate, then `kubectl rollout restart`
  the event services INCLUDING `publisher-adserver`. Disable OrbStack "pause when
  inactive" to prevent recurrence. See project_next_steps.md "E2E RUN GOTCHAS".

---

## Phase 1 — the real bugs (HIGH, do first; #1 and #2 interlock)

### [ ] #1 Ad-pod beacon trace collision  · effort M
**Root cause (verified):** `fillBreak` (`cmd/ssai/main.go`) runs every pod
auction on the SAME request context → `tracing.InjectHTTP` sends the same
traceparent → the SSP (`cmd/ssp/main.go:346`, `TraceIDFromContext`, only mints
fresh when empty) returns the SAME `TraceID` for every pod ad. So all pod ads
share one trace → their win events, impressions AND quartiles collide. Impression
dedup is keyed `("impression", traceID)` (`cmd/tracker/main.go:181`, no creative
id) → a pod of 2+ DISTINCT creatives records only the FIRST impression. Latent
today because the `seen[CreativeID]` guard stops the pod at 1 ad when a
deterministic SSP repeats the creative; bites the moment pods fill with different
ads (the point of pods).

**Fix:** give each pod auction a DISTINCT 32-hex trace, threaded end-to-end.
In the pod loop, mint a fresh trace per ad (crypto/rand, or a deterministic hash
of `session|breakIdx|pod` — keep it 32 hex to match the platform's OTel trace_id
unification), set it as the outbound `traceparent` header on that auction call so
the SSP's win event uses it, and use the same value as `adTrace` for the beacons.
Each pod ad becomes its own win↔impression↔quartile unit. No SSP change needed
(it already honors an inbound traceparent).
- **Files:** `cmd/ssai/main.go` (`fillBreak`, `runAuction`).
- **DECISION (chosen):** fresh independent trace per pod ad (not a child span
  under the manifest trace). Matches "each pod ad is a separate auction/win/
  impression" and needs no billing-key change. (Alternative — child span, one
  trace per manifest request — would force billing to key on trace+creative;
  rejected.)
- **Test:** 2-distinct-creative pod → 2 impressions with distinct traces; billing
  accrues twice. Verify against `TestBilling*` e2e.
- **Risk:** the fresh trace MUST flow into both the auction (win event) and the
  beacons or win↔impression correlation breaks. Note current non-32-hex fallbacks
  (`ssai-{ts}`, `session-bN-pM`) already violate the 32-hex convention — fix those
  too while here.

### [ ] #2 `/v1/t/video` + `/v1/t/audio` lack HMAC / fraud / dedup  · effort M
**Root cause (verified):** those handlers (`cmd/tracker/main.go:449-472`) just
read `tid`/`event`, log, `go publishVideo/publishAudio`, 204 — NO
`ValidateSignature`, NO fraud check, NO `dedup.FirstSeen`, unlike imp/click/conv/
view. Violates the tracker CLAUDE.md ("CRITICAL: every internet-facing request
must validate the sig HMAC + run fraud checks"). Consequences: over-count on
hls.js/dash.js prefetch + seek-back (segmentHandler re-fires quartiles, recorded
every time), spoofable (`GET /v1/t/video?event=complete`), no bot filtering.

**Fix:** extract the shared gate from the impression handler (sig →
fraud → dedup) into a helper and apply to video/audio, with dedup keys
`"video:"+event` / `"audio:"+event` (per-quartile-per-trace: independent
quartiles, one record each). Pairs with #1 (distinct trace per pod ad makes the
key per-ad).
- **Files:** `cmd/tracker/main.go`.
- **PREFLIGHT:** grep every emitter of `/v1/t/video` + `/v1/t/audio` and confirm
  they all HMAC-sign (SSAI: `BuildVideoEventURL`/`BuildAudioEventURL` ✓;
  publisher-adserver VAST ✓) BEFORE enabling validation, else legit beacons 403.
- **Test:** unsigned video beacon 403s (validation on); same quartile twice →
  recorded once. SSAI server-side beacon (Mozilla UA + Referer) passes fraud.

---

## Phase 2 — correctness edges (MEDIUM, independent)

### [ ] #3 Multi-rung DASH has no structural-equality gate  · effort S
`AssembleMultiRung` (`pkg/dash/multirung.go`) assumes every rung has identical
period structure; `serveMultiRungDASH` (`cmd/ssai/multirung.go`) only guarantees
SAME ad decisions, not equal CONTENT-segment counts (each rung reads its own
origin variant). A demuxed/differently-segmented origin → silent period
misalignment / wrong init (the `i < len(segs)` guard hides it).
**Fix:** after building per-rung `Seg` lists, `rungsAligned()` = equal len +
matching per-index `Ad`/`Duration`/init-change positions; else `return false` →
single-rung fallback. Default-off feature, so low blast radius. Test: mismatched
rungs → fallback, no panic.

### [ ] #4 `closeScteBreaks` unclosed break when planned > remaining content · S
`pkg/ssai/manifest.go` — if the cumulative loop reaches playlist end without
hitting `planned`, no CueIn is set → `Breaks()` treats the CUE-OUT as unterminated
and swallows every remaining segment. **Fix:** close at the last segment (cap the
span) so a DATERANGE break never runs to end unbounded. Test: DATERANGE with
planned > content → closes at end.

---

## Phase 3 — robustness (LOW, quick + safe)

- [ ] **#5** `segmentURL` (`cmd/ssai/main.go`): use
  `strings.TrimRight(d.publicURL,"/") + routes.SSAISegment` (siblings already do;
  a trailing-slash `ssai.public_url` yields `//v1/ssai/seg`). Trivial.
- [ ] **#6** `pkg/id3.synchsafe`/`Encode`: guard payload > `0x0FFFFFFF` (28 bits)
  — currently silently truncates the size field → corrupt tag. Return error or
  panic with a clear message. Tiny in the SSAI path but it's a public encoder.
- [ ] **#7** `contentVersion` (`pkg/transcode/conditioner.go`): FNV-32 → **FNV-64**
  (`fnv.New64a`) to shrink the stale-serve collision window. Invalidates existing
  conditioned-ad cache paths once (re-conditions; acceptable). Update
  `TestConditionerCacheHit` (uses `cacheBase`).
- [ ] **#8** DASH `quartileStream` (`pkg/dash/assemble.go`): `Timescale=1000`,
  `presentationTime=int(frac*dur*1000)` — int-second truncation makes sub-second
  quartiles disagree with the HLS `X-QUARTILES` (`ftoa`) precision. Update
  `TestAssembleVODQuartileEvents`.

---

## Verified CORRECT (ruled out — do NOT re-investigate)
`eventsForSegment` quartile math (exhaustive), pod-loop termination, `Profile.Hash`
field coverage, single-flight/cache-key consistency, `fromCache` TS-vs-CMAF init,
`FFmpegArgs` splice byte-compat (`TestGoldenSpliceCompatible`), `ParseLadder`
fallbacks, `#EXT-X-MEDIA` GROUP-ID preservation, audio-channel routing, `Stitch`
back-to-front indexing + contentMap restore, `AddOMID` ad-period-only + default-off
gating, prewarm format selection.

## Branching
Phase 1 on `fix/ssai-review-fixes` (this branch). Phases 2 & 3 can share it or
split — they're independent. Commit per finding; merge Phase 1 first (the real
bugs) once verified on a healthy stack.
