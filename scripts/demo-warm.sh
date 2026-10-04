#!/usr/bin/env bash
#
# demo-warm.sh — bring an ALREADY-RUNNING stack to a fully browsable demo state,
# WITHOUT wiping anything and WITHOUT needing `make demo-forward`. This is the
# non-destructive middle ground between:
#   • demo-setup.sh — full prep + traffic + VERIFY, but assumes demo-forward (localhost)
#   • demo-reset.sh — in-cluster (node-IP) but TRUNCATES all three stores first
#
# Use it when the DB is empty/partial (e.g. an e2e run left it bare) and you just
# want: login working + every ad format + SSAI ads actually rendering — then you'll
# drive traffic / a load test yourself. It STOPS BEFORE generating any traffic.
#
# Talks to the cluster via its LoadBalancer IPs (same as demo-reset), so no
# port-forward. Host prereqs for the prewarm/packager steps: Go toolchain + ffmpeg.
#
# Not `set -e`: run every step, report at the end.
set -uo pipefail
cd "$(dirname "$0")/.."
NS="${DEMO_NAMESPACE:-adtech}"

NODE_IP="$(kubectl -n "$NS" get svc gateway-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)"
[ -z "$NODE_IP" ] && NODE_IP="$(kubectl -n "$NS" get svc minio-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)"
[ -z "$NODE_IP" ] && { echo "✗ could not resolve the node IP from the *-lb services (is the stack up?)"; exit 1; }
DB="postgres://adtech:adtech-local-dev@$NODE_IP:5432/adtech?sslmode=disable"
GW() { curl -sk --resolve gateway.adtech.local:443:"$NODE_IP" "$@"; }
echo "▶ demo-warm against node $NODE_IP (non-destructive — nothing is wiped)"

echo "▶ [0/5] mapping *.adtech.local hostnames in /etc/hosts (incl. the bare adtech.local alias)…"
if [ "${SKIP_HOSTS:-0}" = "1" ]; then
  echo "  ~ SKIP_HOSTS=1 — skipping (run 'make hosts' yourself)"
else
  bash scripts/hosts-setup.sh || echo "  ⚠ couldn't update /etc/hosts — run 'make hosts' (needs sudo)"
fi

# Seed UPSERTs (idempotent) — adds accounts/logins/campaigns/placements/creatives.
# SEED_CREATIVES_URL_BASE points at the branded gateway so stored creative URLs are
# browser-correct even without demo-forward (the serve path host-rewrites anyway, so
# this is belt-and-braces). S3 goes straight to the Minio LB so media uploads land.
echo "▶ [1/5] seeding accounts, logins, campaigns, placements, creatives (UPSERT)…"
S3_ENDPOINT="$NODE_IP:9000" S3_ACCESS_KEY=adtech S3_SECRET_KEY=adtech-local-dev \
  SEED_CREATIVES_URL_BASE="https://gateway.adtech.local/v1/creatives" \
  DATABASE_URL="$DB" \
  go run ./cmd/seed --profile standard || echo "  ⚠ seed reported an issue (may be idempotent no-op)"

echo "▶ [2/5] refreshing DSP/SSP warm caches so services see the seed…"
refreshed=0
for app in dsp-internal ssp exchange adserver reporting; do
  port=8082; case "$app" in ssp) port=8084;; exchange) port=8081;; adserver) port=8085;; reporting) port=8086;; esac
  for pod in $(kubectl get pods -n "$NS" -l app="$app" -o name 2>/dev/null); do
    kubectl exec -n "$NS" "$pod" -- wget -qO- --post-data= "http://localhost:$port/debug/cache/refresh" >/dev/null 2>&1 \
      && refreshed=$((refreshed+1))
  done
done
echo "  ✔ refreshed $refreshed pod cache(s)"

echo "▶ [3/5] SSAI content origin (package only if missing — e.g. after a PURGE)…"
code=$(GW -o /dev/null -w '%{http_code}' --max-time 10 "https://gateway.adtech.local/v1/creatives/ssai/content/sample/master.m3u8" 2>/dev/null)
if [ "$code" = "200" ]; then
  echo "  ✔ present — skip packaging"
elif command -v ffmpeg >/dev/null 2>&1; then
  echo "  … missing (http $code) — packaging a 60s Sintel origin with pre/mid/post breaks…"
  S3_ENDPOINT="$NODE_IP:9000" S3_ACCESS_KEY=adtech S3_SECRET_KEY=adtech-local-dev \
    PACKAGER_SOURCE_KEY=media/sintel-360-1mb.mp4 PACKAGER_TARGET_DURATION_SEC=60 PACKAGER_BREAKS=pre,mid,post \
    DATABASE_URL="$DB" go run ./cmd/content-packager 2>&1 | tail -1 \
    && echo "  ✔ packaged" || echo "  ⚠ packaging failed"
else
  echo "  ⚠ ffmpeg not on host — SSAI content will 404 until packaged"
fi

echo "▶ [4/5] prewarm — condition every video/audio ad creative so SSAI breaks FILL (ffmpeg via transcoder)…"
PREWARM_TRANSCODER_URL="http://$NODE_IP:8094" DATABASE_URL="$DB" \
  go run ./cmd/prewarm 2>&1 | grep -iE "complete|failed|warmed" | tail -2 \
  || echo "  ⚠ prewarm had issues (SSAI may show content instead of an ad until conditioned)"

echo "▶ [5/5] done — NO traffic generated (run 'make loadtest' / 'make traffic' / 'make demo-loadtest' next)."
cat <<EOF

✅ demo-warm complete (non-destructive). Browse via the branded domains:
     https://adtech.local/login   →  admin@adtech.local / admin
     https://viewtube.adtech.local  https://twitchr.adtech.local  (+ soundwave/primereel/chronicle/gadget)

  NOTE: SSAI *ad-segment* URLs + VAST click-throughs use SSAI_PUBLIC_URL / the click
  base, which default to http://localhost:8080. Impressions, quartiles, media files
  and SSAI *content* segments are all on gateway.adtech.local. For those two localhost
  bits to resolve in a standalone browser, either run 'make demo-forward', or set
  SSAI_PUBLIC_URL (+ click base) to https://gateway.adtech.local via helm.
EOF
