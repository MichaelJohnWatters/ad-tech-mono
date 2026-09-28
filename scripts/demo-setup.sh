#!/usr/bin/env bash
#
# demo-setup.sh — one command to make the stack demo-ready AND prove every ad
# format actually serves. Assumes `make demo-forward` is running in another
# terminal (host↔cluster tunnels on the canonical localhost ports).
#
# Not `set -e`: we run ALL steps and report a per-format GREEN/RED at the end,
# rather than aborting on the first hiccup.
set -uo pipefail
cd "$(dirname "$0")/.."

GW="${DEMO_GATEWAY:-http://localhost:8080}"
PUBAD="${DEMO_PUBAD:-http://localhost:8088}"
SSAI="${DEMO_SSAI:-http://localhost:8093}"
REP="${DEMO_REPORTING:-http://localhost:8086}"

echo "▶ [0/6] checking the host↔cluster bridge…"
if ! curl -fsS -o /dev/null --max-time 3 "$GW/healthz"; then
  echo "✗ can't reach $GW — run 'make demo-forward' in another terminal first."
  exit 1
fi
echo "  ✔ gateway reachable"

echo "▶ [0b] ensuring full trace sampling (so the pub-sim trace panel shows Jaeger spans)…"
cur=$(kubectl get deploy exchange -n adtech -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="OTEL_SAMPLE_RATIO")].value}' 2>/dev/null)
if [ "$cur" = "1.0" ]; then
  echo "  ✔ already at 1.0 (no restart)"
else
  echo "  … was '$cur' → setting 1.0 on the serving fleet (rolling restart ~1 min)"
  kubectl set env -n adtech \
    deploy/exchange deploy/ssp deploy/dsp-internal deploy/adserver deploy/tracker \
    deploy/reporting deploy/publisher-adserver deploy/ssai deploy/transcoder \
    deploy/dsp-competitor1 deploy/dsp-competitor2 OTEL_SAMPLE_RATIO=1.0 >/dev/null 2>&1 \
    && kubectl rollout status -n adtech deploy/exchange --timeout=120s >/dev/null 2>&1 \
    || echo "  ⚠ couldn't set sampling (kubectl?) — pub-sim Jaeger rows may stay sparse"
fi

echo "▶ [1/6] seeding accounts, campaigns, placements, creatives…"
S3_ENDPOINT=localhost:9000 S3_ACCESS_KEY=adtech S3_SECRET_KEY=adtech-local-dev \
  SEED_CREATIVES_URL_BASE="$GW/v1/creatives" \
  go run ./cmd/seed --profile standard || echo "  ⚠ seed reported an issue (may be idempotent no-op)"

echo "▶ [2/6] refreshing warm caches so services see the seed…"
for port in 8082 8089 8090 8084 8081 8085 8086; do
  curl -fsS -X POST "http://localhost:$port/debug/cache/refresh" >/dev/null 2>&1 || true
done

echo "▶ [2b] cleanup: drop any leftover schain 'warn' override (the pub-sim now sends a REAL schain, so enforcement can stay strict)…"
kubectl exec -n adtech postgres-0 -- psql -U adtech -d adtech -q -c \
  "DELETE FROM config WHERE key='exchange.schain_enforcement' AND updated_by='demo-setup';" >/dev/null 2>&1 \
  && echo "  ✔ warn override removed — Prebid mode bids via its own schain (passes strict too)" \
  || echo "  ⚠ couldn't clean schain override (non-fatal)"
# NOTE: we intentionally do NOT reject creatives to force a "distinct" SSAI ad —
# that thinned demand → flaky no-fills, and rigging the auction isn't honest. All
# ad demand stays; SSAI fills reliably. The ad can visually match the content
# (both are Big Buck Bunny placeholder clips) — that's a seed-asset artifact; the
# honest proof of SSAI is the stitched manifest + server-side beacons (Network
# tab). A genuinely distinct CONTENT clip is future work (package a real HLS
# origin + point ssai.origin_url at it).

if [ "${PREWARM:-0}" = "1" ]; then
  echo "▶ [3/6] conditioning ALL creatives up-front (prewarm — slow; optional)…"
  DATABASE_URL="${DATABASE_URL:-postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable}" \
    go run ./cmd/prewarm 2>&1 | tail -6 || echo "  ⚠ prewarm had issues"
else
  echo "▶ [3/6] skipping bulk prewarm — SSAI warms on-demand in the verify step (set PREWARM=1 to force)"
fi

echo "▶ [4/6] generating baseline traffic (display/native/video/audio)…"
go run ./cmd/simulator run --profile steady --requests "${DEMO_REQUESTS:-300}" --rps "${DEMO_RPS:-50}" \
  || echo "  ⚠ simulator run had issues"

echo "▶ [5/6] rolling up analytics…"
for lvl in minute hourly daily; do
  curl -fsS -o /dev/null -X POST "$REP/debug/rollup/run?level=$lvl" 2>/dev/null || true
done

echo "▶ [6/6] VERIFY — does each ad format actually serve?"
pass=0; fail=0
check() { # name url [grep-pattern]
  local name="$1" url="$2" pat="${3:-}" body code
  body=$(curl -s --max-time 8 "$url" 2>/dev/null)
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 "$url" 2>/dev/null)
  if [ "$code" = "200" ] && { [ -z "$pat" ] || echo "$body" | grep -qi "$pat"; }; then
    printf "  ✔ %-8s GREEN\n" "$name"; pass=$((pass+1))
  else
    printf "  ✗ %-8s RED (http=%s)\n" "$name" "$code"; fail=$((fail+1))
  fi
}
Q="geo=USA&device=mobile"
check display "$PUBAD/v1/pubad/serve?placement_id=pl-sim-mpu&$Q"
check native  "$PUBAD/v1/pubad/native?placement_id=pl-sim-native&$Q"
check video   "$PUBAD/v1/pubad/video/vast?placement_id=pl-sim-video&$Q" "vast"
check audio   "$PUBAD/v1/pubad/audio?placement_id=pl-sim-audio&$Q" "vast"

# SSAI: fetch the manifest until an ad segment appears (conditioning is async —
# the winning creative conditions on first fetch; usually fills within ~10-30s).
segs=0
for i in $(seq 1 15); do
  segs=$(curl -s --max-time 8 "$SSAI/v1/ssai/manifest.m3u8" 2>/dev/null | grep -c "/v1/ssai/seg" || true)
  [ "$segs" -gt 0 ] && break
  sleep 3
done
if [ "$segs" -gt 0 ]; then
  printf "  ✔ %-8s GREEN (%s ad segments stitched)\n" "ssai" "$segs"; pass=$((pass+1))
else
  printf "  ✗ %-8s RED (manifest never stitched an ad — SSAI content may not be packaged)\n" "ssai"; fail=$((fail+1))
fi

echo
echo "  formats: $pass GREEN / $fail RED"
cat <<EOF

✔ Demo setup complete. Log in as admin@adtech.local / admin.

  The 4 guided demos:   $GW/portal/staff#demos
  Format showcase:      $GW/dev/publisher-simulator     (tab per format)
  Trace Explorer:       $GW/dev/trace-explorer
  Real external page:   DEMOSITE_PORT=9500 go run ./cmd/demosite  → http://localhost:9500

  (If a format is RED: re-run this; SSAI needs packaged content — see scripts/ssai-smoke.sh.)
EOF
