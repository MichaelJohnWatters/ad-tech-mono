#!/usr/bin/env bash
#
# demo-reset.sh — ONE reliable command to reset the local demo to a clean, fully
# working state:
#   1. wipe ClickHouse analytics
#   2. truncate Postgres tenant tables + flush Redis + re-seed (in-cluster, via the
#      gateway's /dev/reset-and-reseed — so media uploads land in Minio correctly,
#      unlike a host `go run ./cmd/seed` which hits localhost:9000)
#   3. refresh the DSP campaign + SSP audience warm caches
#   4. (re)package the SSAI content origin ONLY if missing — e.g. after a full
#      `make stack-down PURGE=1 && make stack-up` wiped Minio
#   5. prewarm ad-creative conditioning (video/audio incl. the branded spots)
#   6. verify the first-party-audience demo (Lumière wins for the seeded persona)
#
# Talks to the cluster via its LoadBalancer IPs, so it does NOT need
# `make demo-forward`. Host prereqs for steps 4–5: Go toolchain + ffmpeg.
set -uo pipefail
cd "$(dirname "$0")/.."
NS="${DEMO_NAMESPACE:-adtech}"

NODE_IP="$(kubectl -n "$NS" get svc gateway-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)"
[ -z "$NODE_IP" ] && NODE_IP="$(kubectl -n "$NS" get svc minio-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)"
[ -z "$NODE_IP" ] && { echo "✗ could not resolve the node IP from the *-lb services (is the stack up?)"; exit 1; }
DB="${DATABASE_URL:-postgres://adtech:adtech-local-dev@$NODE_IP:5432/adtech?sslmode=disable}"
GW() { curl -sk --resolve gateway.adtech.local:443:"$NODE_IP" "$@"; }
echo "▶ demo-reset against node $NODE_IP"

echo "▶ [1/6] truncating ClickHouse analytics…"
for t in $(kubectl -n "$NS" exec clickhouse-0 -- clickhouse-client -q "SHOW TABLES FROM adtech" 2>/dev/null); do
  kubectl -n "$NS" exec clickhouse-0 -- clickhouse-client -q "TRUNCATE TABLE IF EXISTS adtech.\`$t\`" 2>/dev/null || true
done

echo "▶ [2/6] Postgres truncate + Redis flush + in-cluster re-seed…"
GW -X POST https://gateway.adtech.local/dev/reset-and-reseed -o /dev/null -w '   reseed HTTP %{http_code}\n' \
  || { echo "✗ reseed call failed"; exit 1; }

echo "▶ [3/6] refreshing warm caches (DSP campaigns + SSP audience)…"
for p in $(kubectl -n "$NS" get pods -l app=dsp-internal -o name 2>/dev/null); do
  kubectl -n "$NS" exec "$p" -- wget -qO- --post-data= http://localhost:8082/debug/cache/refresh >/dev/null 2>&1 || true
done
for p in $(kubectl -n "$NS" get pods -l app=ssp -o name 2>/dev/null); do
  kubectl -n "$NS" exec "$p" -- wget -qO- --post-data= http://localhost:8084/debug/audience/refresh >/dev/null 2>&1 || true
done

echo "▶ [4/6] SSAI content origin (package only if missing — e.g. after a PURGE)…"
code="$(GW -o /dev/null -w '%{http_code}' https://gateway.adtech.local/v1/creatives/ssai/content/sample/master.m3u8)"
if [ "$code" = "200" ]; then
  echo "   present — skip packaging"
elif command -v ffmpeg >/dev/null 2>&1; then
  echo "   missing (http $code) — packaging a 60s Sintel origin with pre/mid/post breaks…"
  S3_ENDPOINT="$NODE_IP:9000" S3_ACCESS_KEY=adtech S3_SECRET_KEY=adtech-local-dev \
    PACKAGER_SOURCE_KEY=media/sintel-360-1mb.mp4 PACKAGER_TARGET_DURATION_SEC=60 PACKAGER_BREAKS=pre,mid,post \
    DATABASE_URL="$DB" go run ./cmd/content-packager 2>&1 | tail -1 \
    || echo "   ⚠ packaging failed — Twitchr/SSAI content will 404 until packaged"
else
  echo "   ⚠ ffmpeg not on host — Twitchr/SSAI content will 404 until packaged (run make demo-setup)"
fi

echo "▶ [5/6] prewarm ad-creative conditioning (branded video/audio spots)…"
PREWARM_TRANSCODER_URL="http://$NODE_IP:8094" DATABASE_URL="$DB" \
  go run ./cmd/prewarm 2>&1 | grep -iE "complete|failed" | tail -1 || echo "   ⚠ prewarm had issues"

echo "▶ [6/6] verify (give the membership cache its 3s drain first)…"
sleep 4
HE="$(printf '%s' demo.shopper@example.com | shasum -a 256 | awk '{print $1}')"
win="$(curl -s "http://$NODE_IP:8084/v1/ssp/serve?placement_id=pl-news-mpu&hashed_email=$HE&geo=US&device=desktop" | grep -oE 'advid=7744054e[a-f0-9-]*' | head -1)"
if [ -n "$win" ]; then
  echo "   ✔ first-party audience: Lumière Diamonds wins for the seeded persona"
else
  echo "   ✗ Lumière did NOT win for the seeded persona — re-run step 3, or check the audience preload"
fi
echo "✅ demo-reset complete"
