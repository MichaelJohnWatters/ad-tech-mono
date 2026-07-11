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

echo "▶ waiting for the stack ($GW)…"
for i in $(seq 1 30); do
  if curl -fsS -o /dev/null "$GW/healthz" 2>/dev/null; then break; fi
  if [ "$i" = 30 ]; then echo "✗ gateway not reachable — is 'tilt up' running?"; exit 1; fi
  sleep 2
done

echo "▶ [1/3] seeding accounts, logins, campaigns, placements, deals…"
S3_ENDPOINT=localhost:9000 S3_ACCESS_KEY=adtech S3_SECRET_KEY=adtech-local-dev \
  SEED_CREATIVES_URL_BASE="$GW/v1/creatives" \
  go run ./cmd/seed --profile standard

echo "▶ [2/3] generating $REQ realistic auctions at ~$RPS rps (impressions/clicks/views)…"
go run ./cmd/simulator run --requests "$REQ" --rps "$RPS"

echo "▶ [3/3] rolling up analytics…"
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
