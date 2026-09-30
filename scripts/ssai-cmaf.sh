#!/usr/bin/env bash
#
# ssai-cmaf.sh — OPT-IN: make the SSAI "Video · DASH (CMAF)" demo path real.
#
# The platform supports both HLS/MPEG-TS (the default, production-common) and the
# modern CMAF/fMP4 path that unifies HLS+DASH. The default `make demo-setup` only
# packages the TS-HLS content, so the pub-sim's "Video · DASH (CMAF)" option shows
# content-only until an async warm catches up. This script exercises the CMAF path
# properly so that option plays real stitched ads immediately:
#   1. Package a CMAF/fMP4 content origin (sample-cmaf) with pre/mid/post breaks.
#   2. Warm the fMP4 ad conditioning (the transcoder must produce .m4s segments
#      byte-compatible with the CMAF content — a different profile than the TS one).
#   3. Verify the DASH multi-period MPD actually splices ad periods.
#
# Run AFTER `make demo-setup` (which sets the bunny-wins-the-break demand + slate).
# Kept OPT-IN so the default demo stays fast (CMAF packaging is ~90s of ffmpeg).
#
#   make demo-cmaf         # this script
# Needs `make demo-forward` running.
set -uo pipefail
cd "$(dirname "$0")/.."

GW="${DEMO_GATEWAY:-http://localhost:8080}"

echo "▶ [0/3] checking the host↔cluster bridge…"
curl -fsS -o /dev/null --max-time 3 "$GW/healthz" || { echo "✗ can't reach $GW — run 'make demo-forward' first."; exit 1; }

echo "▶ [1/3] packaging a CMAF/fMP4 content origin (sample-cmaf: 60s Sintel, pre/mid/post; ffmpeg ~90s)…"
S3_ENDPOINT=localhost:9000 S3_ACCESS_KEY=adtech S3_SECRET_KEY=adtech-local-dev \
PACKAGER_SOURCE_KEY=media/sintel-360-1mb.mp4 \
PACKAGER_CONTENT_ID=sample-cmaf \
PACKAGER_CONTAINER=cmaf \
PACKAGER_TARGET_DURATION_SEC=60 \
PACKAGER_BREAKS=pre,mid,post \
DATABASE_URL="${DATABASE_URL:-postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable}" \
  go run ./cmd/content-packager 2>&1 | tail -1 \
  && echo "  ✔ sample-cmaf packaged (fMP4 .m4s + init.mp4, HLS+DASH-compatible)" \
  || echo "  ⚠ packaging failed (needs ffmpeg on host)"

echo "▶ [2/3] warming fMP4 ad conditioning — fetch the DASH manifest so the auction runs and the"
echo "        winning ad conditions to CMAF (cache-first: SSAI never blocks, so the first plays keep"
echo "        content until the async warm lands ~30s later)…"
ORIGIN="$GW/v1/creatives/ssai/content/sample-cmaf/master.m3u8"
OENC=$(python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" "$ORIGIN")
URL="$GW/v1/ssai/manifest.mpd?placement_id=pl-sim-video&geo=USA&device=ctv&origin=$OENC"
# The break auction rotates across several eligible bunny creatives, and EACH needs
# its own fMP4 conditioning (a different transcode profile than the TS one prewarm
# does). So keep fetching until fill is STABLE (3 fetches in a row all filled) — that
# means every creative that can win a break has been conditioned to CMAF and cached.
stable=0
for i in $(seq 1 30); do
  ads=$(curl -s --max-time 8 "$URL" 2>/dev/null | grep -c '/v1/ssai/seg' || true)
  if [ "$ads" -gt 0 ]; then stable=$((stable+1)); else stable=0; fi
  printf "  t=%3ds → ad segments: %-2s  (consecutive fills: %s/3)\n" "$(((i-1)*10))" "$ads" "$stable"
  [ "$stable" -ge 3 ] && break
  sleep 10
done

echo "▶ [3/3] VERIFY — does the DASH multi-period MPD splice ads?"
MPD=$(curl -s --max-time 10 "$URL" 2>/dev/null)
PER=$(echo "$MPD" | grep -c '<Period' || true)
ADS=$(echo "$MPD" | grep -c '/v1/ssai/seg' || true)
EVS=$(echo "$MPD" | grep -c 'EventStream' || true)
if [ "$ADS" -gt 0 ]; then
  echo "  ✔ CMAF/DASH GREEN — $PER periods · $ADS ad segments · $EVS quartile EventStreams"
else
  echo "  ✗ CMAF/DASH RED — no ad periods (still warming? re-run; or check: kubectl logs -n adtech -l app=ssai | grep -i condition)"
fi

cat <<EOF

✔ CMAF/DASH ready. Show it in the pub-sim:
  $GW/dev/publisher-simulator  → SSAI (CTV) tab → format "Video · DASH (CMAF)" → Play
  dash.js walks the multi-period MPD; the SAME bunny ad — conditioned as fMP4 this
  time — splices into the Sintel content. (HLS/TS is the default tab; this is the
  modern CMAF path that unifies HLS + DASH.)
EOF
