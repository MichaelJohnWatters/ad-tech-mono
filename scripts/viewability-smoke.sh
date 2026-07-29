#!/usr/bin/env bash
# Real browser-driven IAB video viewability smoke.
#
# Proves the ONE client-side step the HTTP-only e2e harness can't: a real browser
# renders the demo publisher's video page, its IntersectionObserver self-measures
# >=50%-on-screen-for-2s, fires the signed viewability beacon, and that lands in
# ClickHouse as a channel=video, iab_viewable=1 view — the full chain
# browser -> tracker -> NATS -> reporting -> ClickHouse.
#
# Opt-in (make viewability-smoke). Needs: the live stack up + seeded, Google
# Chrome installed, and the localhost LB ports (8088 pubad, 8080 gateway) up
# (run `make stack-doctor` first if the tunnels are dead).
set -euo pipefail
cd "$(dirname "$0")/.."

NS="${DEMO_NAMESPACE:-adtech}"
PUBAD="${DEMOSITE_PUBAD_URL:-http://localhost:8088}"
GATEWAY="${DEMOSITE_MEDIA_URL:-http://localhost:8080}"
PORT="${DEMOSITE_PORT:-9000}"
PAGE="${VIEWABILITY_PAGE:-/p/video-hub}"
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

say() { echo "▶ $*"; }
ch() { kubectl -n "$NS" exec clickhouse-0 -- clickhouse-client -q "$1" 2>/dev/null; }

say "[1/6] preflight (chrome, stack, clickhouse)…"
[ -x "$CHROME" ] || { echo "✗ Google Chrome not found at: $CHROME"; exit 1; }
curl -fsS -m5 -o /dev/null "$PUBAD/healthz" || { echo "✗ pubad unreachable at $PUBAD — run make stack-doctor"; exit 1; }
curl -fsS -m5 -o /dev/null "$GATEWAY/healthz" || { echo "✗ gateway unreachable at $GATEWAY — run make stack-doctor"; exit 1; }
ch "SELECT 1" >/dev/null || { echo "✗ clickhouse unreachable"; exit 1; }

BEFORE="$(ch "SELECT count() FROM adtech.views WHERE channel='video' AND iab_viewable=1")"
BEFORE="${BEFORE:-0}"
say "[2/6] baseline: $BEFORE video-viewable views in ClickHouse"

say "[3/6] starting demo publisher (:$PORT) pointed at the live stack…"
DEMOSITE_PUBAD_URL="$PUBAD" \
DEMOSITE_SDK_URL="$GATEWAY/static/adtech.js" \
DEMOSITE_MEDIA_URL="$GATEWAY" \
DEMOSITE_PORT="$PORT" \
  go run ./cmd/demosite >/tmp/viewability-demosite.log 2>&1 &
DEMOPID=$!
trap 'kill $DEMOPID 2>/dev/null || true' EXIT
for i in $(seq 1 20); do curl -fsS -m2 -o /dev/null "http://localhost:$PORT/healthz" 2>/dev/null && break; sleep 0.5; done

say "[4/6] driving headless Chrome through http://localhost:$PORT$PAGE …"
go run ./cmd/viewabilitysmoke -url "http://localhost:$PORT$PAGE" -headless=true -dwell=8s

say "[5/6] waiting for the async NATS -> reporting -> ClickHouse write…"
AFTER="$BEFORE"
for i in $(seq 1 20); do
  AFTER="$(ch "SELECT count() FROM adtech.views WHERE channel='video' AND iab_viewable=1")"; AFTER="${AFTER:-0}"
  [ "$AFTER" -gt "$BEFORE" ] && break
  sleep 1
done

say "[6/6] result: video-viewable views $BEFORE -> $AFTER"
if [ "$AFTER" -gt "$BEFORE" ]; then
  echo "✓ PASS — a real browser's IntersectionObserver fired the video viewability beacon and it reached ClickHouse (iab_viewable=1)"
else
  echo "✗ FAIL — no new channel=video iab_viewable view landed. Check /tmp/viewability-demosite.log and that pl-sim-video fills under the current enforcement."
  exit 1
fi
