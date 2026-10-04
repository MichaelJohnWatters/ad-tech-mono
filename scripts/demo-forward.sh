#!/usr/bin/env bash
#
# demo-forward.sh — bridge the host to the Rancher/k3s cluster for SETUP / TOOLING.
#
# SETUP/TOOLING ONLY — you do NOT need this to browse or record the demo. Every
# browser-facing surface (all demo sites incl. Twitchr/SSAI ads + click-throughs,
# the portal, the shop, trackers) works through the https://*.adtech.local ingress
# with no bridge. This exists purely so HOST-SIDE CLI TOOLS that hardcode
# localhost:<port> can reach the ClusterIP services: scripts/demo.sh / demo-setup.sh,
# cmd/simulator (make traffic / loadtest), cmd/prewarm, the e2e suite, ssai-smoke.sh.
# Run in its OWN terminal; Ctrl-C tears every tunnel down.
#
#   make demo-forward      # this script  (leave running)
#   make demo-setup        # in a second terminal
#
set -uo pipefail
NS="${DEMO_NS:-adtech}"

# svc:localPort:remotePort — localPort matches each tool's hardcoded default.
FORWARDS=(
  gateway:8080:8080
  exchange:8081:8081
  dsp-internal:8082:8082
  tracker:8083:8083
  ssp:8084:8084
  adserver:8085:8085
  reporting:8086:8086
  publisher-adserver:8088:8088
  dsp-competitor1:8089:8089
  dsp-competitor2:8090:8090
  ssai:8093:8093
  transcoder:8094:8094
  postgres:5432:5432
  redis:6379:6379
  minio:9000:9000
  grafana:3000:3000
  jaeger:16686:16686
  prometheus:9090:9090
)

PIDS=()
cleanup() {
  echo; echo "▶ tearing down port-forwards…"
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  echo "✔ done"
}
trap cleanup INT TERM EXIT

command -v kubectl >/dev/null || { echo "✗ kubectl not found"; exit 1; }
if ! kubectl get ns "$NS" >/dev/null 2>&1; then
  echo "✗ namespace '$NS' not reachable — is Rancher Desktop up?"
  echo "  start it with:  rdctl start   (then wait ~1 min for k3s)"
  exit 1
fi

echo "▶ opening port-forwards to the '$NS' namespace…"
for f in "${FORWARDS[@]}"; do
  svc="${f%%:*}"; rest="${f#*:}"; lport="${rest%%:*}"; rport="${rest##*:}"
  kubectl port-forward -n "$NS" "svc/$svc" "$lport:$rport" >"/tmp/pf-$svc.log" 2>&1 &
  PIDS+=($!)
  printf "  %-20s → localhost:%s\n" "$svc" "$lport"
done

echo "▶ waiting for key tunnels to accept connections…"
for probe in "8080 gateway" "8088 publisher-adserver" "8093 ssai"; do
  set -- $probe; port="$1"; name="$2"
  ok=""
  for i in $(seq 1 20); do
    if curl -fsS -o /dev/null --max-time 2 "http://localhost:$port/healthz" 2>/dev/null; then
      echo "  ✔ $name"; ok=1; break
    fi
    sleep 1
  done
  [ -n "$ok" ] || echo "  ⚠ $name not answering on :$port yet (check /tmp/pf-$name.log)"
done

cat <<EOF

✔ Port-forwards are live. Leave THIS terminal open for the whole demo.

  Next (new terminal):   make demo-setup            # seed + warm + verify formats
  Portal:                http://localhost:8080/portal/staff#demos   (admin@adtech.local / admin)
  Format showcase:       http://localhost:8080/dev/publisher-simulator
  Trace Explorer:        http://localhost:8080/dev/trace-explorer

  Real external site:    minio holds :9000, so run the demosite on another port:
                         DEMOSITE_PORT=9500 go run ./cmd/demosite   → http://localhost:9500

▶ holding tunnels open (Ctrl-C to stop)…
EOF

wait
