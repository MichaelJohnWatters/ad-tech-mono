#!/usr/bin/env bash
#
# demo.sh — the ONE command that puts the local stack into a rich, shared,
# testable state. Everyone runs this, so everyone sees the same thing:
#
#   1. Seeds accounts / logins / campaigns / placements / deals / direct-sold
#      (profiles/*). Every advertiser + publisher account is loginable
#      (<slug>@adtech.local / admin).
#   2. Generates a baseline of REALISTIC attributed traffic — real auctions →
#      real winner's impression/click/view (not placeholders) — so every portal
#      shows live spend / fill / earnings.
#   3. Rolls up analytics so the rolled-up views are populated too.
#
# Requires `tilt up` to be running (services reachable on localhost).
# Run via `make demo`, the Tilt "demo" button, or directly.
#
# Tunables (env): DEMO_REQUESTS (default 800), DEMO_RPS (50),
#                 DEMO_GATEWAY (http://localhost:8080), DEMO_REPORTING (http://localhost:8086)
set -euo pipefail
cd "$(dirname "$0")/.."

GW="${DEMO_GATEWAY:-http://localhost:8080}"
REP="${DEMO_REPORTING:-http://localhost:8086}"
REQ="${DEMO_REQUESTS:-800}"
RPS="${DEMO_RPS:-50}"

# Wait for the serving stack the simulator hits directly (exchange :8081,
# tracker :8083) plus the gateway (:8080). Generous timeout so this survives an
# auto-run on `tilt up` while images are still building.
EXCHANGE="${DEMO_EXCHANGE:-http://localhost:8081}"
TRACKER="${DEMO_TRACKER:-http://localhost:8083}"
echo "▶ waiting for the stack (gateway/exchange/tracker)…"
for i in $(seq 1 90); do
  if curl -fsS -o /dev/null "$GW/healthz" 2>/dev/null \
     && curl -fsS -o /dev/null "$EXCHANGE/healthz" 2>/dev/null \
     && curl -fsS -o /dev/null "$TRACKER/healthz" 2>/dev/null; then break; fi
  if [ "$i" = 90 ]; then echo "✗ stack not reachable — is 'tilt up' running / done building?"; exit 1; fi
  sleep 2
done

echo "▶ [1/4] seeding accounts, logins, campaigns, placements, deals…"
S3_ENDPOINT=localhost:9000 S3_ACCESS_KEY=adtech S3_SECRET_KEY=adtech-local-dev \
  SEED_CREATIVES_URL_BASE="$GW/v1/creatives" \
  go run ./cmd/seed --profile standard

# Warm caches (campaigns/placements/creatives) must reload the freshly-seeded
# data before auctions run — otherwise (esp. right after a reset that truncated
# everything) the caches are empty and every auction no-bids. Synchronous refresh
# beats waiting on the 30s poll. DSP :8082, competitors :8089/:8090, SSP :8084,
# exchange :8081, adserver :8085, reporting :8086.
echo "▶ [2/4] refreshing warm caches so services see the seed…"
for port in 8082 8089 8090 8084 8081 8085 8086; do
  curl -fsS -X POST "http://localhost:$port/debug/cache/refresh" >/dev/null 2>&1 || true
done

echo "▶ [3/4] generating $REQ realistic auctions at ~$RPS rps (display/native/video/audio)…"
go run ./cmd/simulator run --profile steady --requests "$REQ" --rps "$RPS"

echo "▶ [4/4] rolling up analytics…"
for lvl in minute hourly daily; do
  curl -fsS -o /dev/null -X POST "$REP/debug/rollup/run?level=$lvl" 2>/dev/null || true
done

cat <<EOF

✔ Demo ready — open $GW/portal/advertiser
  • Log in as any seeded account (emails printed above), password: admin
    e.g. adv-acme@adtech.local (advertiser) · tech-review@adtech.local (publisher)
  • Staff console: admin@adtech.local — use "Impersonate" to view any account
  • For ongoing live traffic: run the 'sim-continuous' Tilt resource, or \`make traffic\`
EOF
