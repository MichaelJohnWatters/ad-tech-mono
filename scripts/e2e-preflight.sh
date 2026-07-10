#!/usr/bin/env bash
# e2e-preflight: the e2e harness verifies analytics via the reporting service's
# in-memory /debug read-back endpoints, which only exist on the `memory`
# analytics backend (ADR 0001: "memory — Default; used by unit tests, CI, and
# the e2e harness's debug read-backs"). The tilt stack deploys reporting on
# `clickhouse` for interactive local dev, so pin it to `memory` before an e2e
# run — otherwise every analytics/billing assertion times out (read-back 501s).
#
# Best-effort: skips cleanly when kubectl or the deployment isn't present (e.g.
# CI running the harness against a differently-provisioned stack).
set -euo pipefail
NS="${ADTECH_NS:-adtech}"

if ! command -v kubectl >/dev/null 2>&1; then
  echo "[e2e-preflight] kubectl not found; skipping analytics-backend check"; exit 0
fi
if ! kubectl get deploy reporting -n "$NS" >/dev/null 2>&1; then
  echo "[e2e-preflight] reporting deployment not found in ns/$NS; skipping"; exit 0
fi

cur=$(kubectl get deploy reporting -n "$NS" \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="REPORTING_ANALYTICS_BACKEND")].value}' 2>/dev/null || true)
if [ "$cur" = "memory" ]; then
  echo "[e2e-preflight] reporting already on the memory backend"; exit 0
fi

echo "[e2e-preflight] pinning reporting to the memory backend for e2e (was '${cur:-<default>}'; ADR 0001)"
kubectl set env deployment/reporting -n "$NS" REPORTING_ANALYTICS_BACKEND=memory >/dev/null
kubectl rollout status deployment/reporting -n "$NS" --timeout=120s
echo "[e2e-preflight] reporting ready on the memory backend"
