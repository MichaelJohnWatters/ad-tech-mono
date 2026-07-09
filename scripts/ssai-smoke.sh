#!/usr/bin/env bash
# ssai-smoke.sh — R1 live smoke for the SSAI ad-conditioning pipeline.
#
# Runs against the deployed stack (tilt up on OrbStack). It fetches a stitched
# SSAI manifest (which drives a real auction → real transcoder → Minio), follows
# a stitched ad segment through its server-side beacon redirect, and asserts the
# downloaded segment is a real, decodable conditioned ad (ffmpeg). This is the
# end-to-end proof the unit + golden tests can't give: real services, real
# object store, real ffmpeg-in-pod.
#
# Requires: a running stack (ssai :8093, gateway :8080) and ffmpeg/ffprobe on the
# host. Exits non-zero on any failure.
set -euo pipefail

SSAI="${SSAI_URL:-http://localhost:8093}"
GATEWAY="${GATEWAY_URL:-http://localhost:8080}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "==> health"
for u in "$SSAI/readyz" "$GATEWAY/readyz"; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$u")
  [ "$code" = "200" ] || { echo "FAIL: $u => $code"; exit 1; }
done

echo "==> fetch stitched manifest (real auction + conditioning)"
# A few attempts: on a fully-cold cache the first fetch warms async + keeps
# content; the ad segments appear once conditioning completes.
segs=0
for i in $(seq 1 6); do
  curl -s "$SSAI/v1/ssai/manifest.m3u8" -o "$WORK/m.m3u8"
  segs=$(grep -c "/v1/ssai/seg" "$WORK/m.m3u8" || true)
  echo "   attempt $i: ad-segment URLs = $segs"
  [ "$segs" -gt 0 ] && break
  sleep 3
done
[ "$segs" -gt 0 ] || { echo "FAIL: manifest never stitched an ad"; cat "$WORK/m.m3u8"; exit 1; }

echo "==> follow first ad segment beacon → conditioned .ts"
segline=$(grep "/v1/ssai/seg" "$WORK/m.m3u8" | head -1)
url=$(echo "$segline" | sed -E "s#^https?://[^/]+#$SSAI#")
curl -sL "$url" -o "$WORK/ad.ts" -w '   final http=%{http_code} bytes=%{size_download} type=%{content_type}\n'

echo "==> ffprobe + decode the conditioned segment"
ffprobe -v error -show_entries stream=codec_type,codec_name,width,height \
  -show_entries format=duration -of default=noprint_wrappers=1 "$WORK/ad.ts"
ffmpeg -v error -i "$WORK/ad.ts" -f null - || { echo "FAIL: segment did not decode"; exit 1; }

echo "==> PASS: real stack conditioned a decodable ad segment end-to-end"
