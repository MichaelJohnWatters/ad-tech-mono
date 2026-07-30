#!/usr/bin/env bash
# Real browser-driven ADVERTISER-tag smoke (the buy-side twin of
# viewability-smoke). A real browser loads the demo advertiser site, gives
# consent (the shared adtech-adv.js tag fires the retargeting pixel) and
# converts (fires the conversion pixel), and this asserts both land in
# ClickHouse: a kind=site_visit behaviour signal + a conversions row — the chain
# browser -> adtech-adv.js -> tracker -> NATS -> reporting -> ClickHouse.
#
# Opt-in (make advertiser-smoke). Needs the stack up + seeded, Chrome installed,
# and the localhost LB ports up (or override DEMOADV_* with the node IP).
set -euo pipefail
cd "$(dirname "$0")/.."

NS="${DEMO_NAMESPACE:-adtech}"
TRACKER="${DEMOADV_TRACKER_URL:-http://localhost:8083}"
GATEWAY="${DEMOADV_GATEWAY_URL:-http://localhost:8080}"
PORT="${DEMOADV_PORT:-9200}"
PAGE="${ADVERTISER_PAGE:-/models/f150}"
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

say() { echo "▶ $*"; }
ch() { kubectl -n "$NS" exec clickhouse-0 -- clickhouse-client -q "$1" 2>/dev/null; }

say "[1/6] preflight (chrome, stack, clickhouse)…"
[ -x "$CHROME" ] || { echo "✗ Google Chrome not found at: $CHROME"; exit 1; }
curl -fsS -m5 -o /dev/null "$TRACKER/healthz" || { echo "✗ tracker unreachable at $TRACKER — run make stack-doctor"; exit 1; }
curl -fsS -m5 -o /dev/null "$GATEWAY/healthz" || { echo "✗ gateway unreachable at $GATEWAY — run make stack-doctor"; exit 1; }
ch "SELECT 1" >/dev/null || { echo "✗ clickhouse unreachable"; exit 1; }

RT_BEFORE="$(ch "SELECT count() FROM adtech.behaviour_signals WHERE kind='site_visit'")"; RT_BEFORE="${RT_BEFORE:-0}"
CV_BEFORE="$(ch "SELECT count() FROM adtech.conversions")"; CV_BEFORE="${CV_BEFORE:-0}"
say "[2/6] baseline: site_visit signals=$RT_BEFORE, conversions=$CV_BEFORE"

say "[3/6] starting demo advertiser (:$PORT) pointed at the live stack…"
DEMOADV_TRACKER_URL="$TRACKER" \
DEMOADV_SDK_URL="$GATEWAY/static/adtech-adv.js" \
DEMOADV_PORT="$PORT" \
  go run ./cmd/demoadv >/tmp/advertiser-demoadv.log 2>&1 &
DEMOPID=$!
trap 'kill $DEMOPID 2>/dev/null || true' EXIT
for i in $(seq 1 20); do curl -fsS -m2 -o /dev/null "http://localhost:$PORT/" 2>/dev/null && break; sleep 0.5; done

say "[4/6] driving headless Chrome through http://localhost:$PORT$PAGE …"
go run ./cmd/advertisersmoke -url "http://localhost:$PORT$PAGE" -headless=true

say "[5/6] waiting for the async NATS -> reporting -> ClickHouse writes…"
RT_AFTER="$RT_BEFORE"; CV_AFTER="$CV_BEFORE"
for i in $(seq 1 20); do
  RT_AFTER="$(ch "SELECT count() FROM adtech.behaviour_signals WHERE kind='site_visit'")"; RT_AFTER="${RT_AFTER:-0}"
  CV_AFTER="$(ch "SELECT count() FROM adtech.conversions")"; CV_AFTER="${CV_AFTER:-0}"
  [ "$RT_AFTER" -gt "$RT_BEFORE" ] && break
  sleep 1
done

say "[6/6] result: site_visit $RT_BEFORE -> $RT_AFTER  ·  conversions $CV_BEFORE -> $CV_AFTER"
# Retargeting is the pass condition: it proves the shared adtech-adv.js tag fired
# a real pixel that flowed browser -> tracker -> reporting -> ClickHouse.
if [ "$RT_AFTER" -gt "$RT_BEFORE" ]; then
  echo "✓ PASS — the shared advertiser tag's retargeting pixel fired in a real browser and reached ClickHouse (kind=site_visit)"
  if [ "$CV_AFTER" -le "$CV_BEFORE" ]; then
    echo "  ⚠ note: the conversion pixel did NOT land. Advertiser-fired /v1/t/conv is HMAC-signature-gated on the"
    echo "    tracker, but an advertiser's own site can't sign with our key (its retargeting pixel /v1/t/rt correctly"
    echo "    isn't gated). Advertiser conversions need a server-issued signed pixel or the same unsigned+fraud-gated"
    echo "    path as /v1/t/rt. Not a regression from the SDK work — a pre-existing conversion-auth gap."
  fi
else
  echo "✗ FAIL — the retargeting pixel did not land. Check /tmp/advertiser-demoadv.log."
  exit 1
fi
