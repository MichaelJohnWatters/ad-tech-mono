#!/bin/bash
# scripts/colima-recover.sh — restart Colima + wait for k3s + bounce local
# services so the platform is fully reachable again.
#
# Idempotent: safe to run when Colima is up, partially up, or fully down.
# Used by Claude Code to recover the local stack mid-session when the
# macOS vz vmType wedges (a recurring issue on macOS 14). Also useful
# from a terminal whenever `kubectl` or `curl localhost:8080/readyz`
# returns ECONNREFUSED.
#
# Recovery path:
#
#   1. colima stop --force       (no-op if already stopped)
#   2. colima start --kubernetes (uses the saved default profile config)
#   3. Wait for k3s API
#   4. Wait for postgres/nats/redis pods Ready
#   5. Trigger Tilt rebuilds of the local-process services so they
#      re-connect with the freshly-reachable infra
#   6. Poll gateway /readyz until 200
#
# Exit codes:
#   0 — fully recovered, gateway reachable
#   1 — recovery failed; manual intervention needed
set -uo pipefail

log() { printf '%s\n' "[colima-recover] $*"; }
fail() { log "FAILED: $*"; exit 1; }

# Wait for a command to succeed, polling every $1 seconds up to $2 attempts.
wait_for() {
    local sleep_s=$1 max=$2 desc=$3
    shift 3
    for i in $(seq 1 "$max"); do
        if "$@" >/dev/null 2>&1; then
            log "✓ $desc (after ${i}x ${sleep_s}s)"
            return 0
        fi
        sleep "$sleep_s"
    done
    log "✗ $desc — timed out after $((sleep_s * max))s"
    return 1
}

log "step 1/6: stopping Colima (if running)"
colima stop --force 2>/dev/null || true

log "step 2/6: starting Colima with Kubernetes"
# Profile defaults (cpu/memory/disk/vmType) live in ~/.colima/default/colima.yaml
# so we don't override them here — keeps the script working if the user
# changes those settings.
colima start --kubernetes || fail "colima start"

log "step 3/6: waiting for k3s API"
wait_for 2 30 "k3s API reachable" \
    kubectl --context colima get nodes \
    || fail "k3s API never came up"

log "step 4/6: waiting for infra pods (postgres / nats / redis)"
# --timeout on `kubectl wait` is per-pod, not per-resource; if one of them
# is in CrashLoopBackOff that timeout will trigger.
for app in postgres nats redis; do
    if ! kubectl --context colima -n adtech wait --for=condition=Ready pod \
            -l app=$app --timeout=120s >/dev/null 2>&1; then
        log "warn: $app pod did not reach Ready in 120s — continuing anyway"
    fi
done

log "step 5/6: triggering Tilt rebuilds of local-process services"
if curl -sf http://localhost:10350/api/view >/dev/null 2>&1; then
    # Tilt's local_resource processes don't auto-restart when Postgres
    # comes back. They start in a connect-once-at-boot pattern (e.g.
    # gateway opens a *sql.DB at startup; if Postgres was down then,
    # the handle stays nil for the rest of the process lifetime).
    # Triggering a rebuild via the Tilt API re-runs the serve_cmd with
    # a fresh process.
    for svc in gateway dsp dsp-competitor1 dsp-competitor2 ssp \
               exchange adserver tracker reporting publisher-adserver; do
        curl -s -X POST "http://localhost:10350/api/trigger" \
            -H 'Content-Type: application/json' \
            -d "{\"manifest_names\":[\"$svc\"],\"build_reason\":16}" \
            -o /dev/null || true
    done
    log "✓ triggered Tilt rebuilds for 10 services"
else
    log "warn: Tilt UI not reachable at :10350 — start it with 'tilt up'"
fi

log "step 6/6: polling gateway /readyz"
if wait_for 2 60 "gateway /readyz=200" \
        bash -c "curl -sf -o /dev/null http://localhost:8080/readyz"; then
    log "✅ recovery complete"
    exit 0
else
    log "gateway never came back; check 'tilt up' output for errors"
    exit 1
fi
