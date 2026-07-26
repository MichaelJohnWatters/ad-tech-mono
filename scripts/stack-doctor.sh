#!/usr/bin/env bash
# stack-doctor.sh — staged diagnosis + repair for the local Rancher Desktop
# stack. Encodes the post-sleep failure tree debugged live on 2026-07-26 so
# recovery is one idempotent command instead of an hour of archaeology:
#
#   Stage 1  RD app/VM up?       app dead → rdctl start; app up but VM wedged
#                                (docker sock + k8s API both dead) → clean
#                                rdctl shutdown/start. The ONLY big hammer.
#   Stage 2  k8s API answering?  wait
#   Stage 3  adtech pods Ready?  wait (they restart in dependency order)
#   Stage 4  localhost tunnels?  THE SIGNATURE: in-cluster ClusterIP works but
#                                localhost/hostPort connections RESET. Cause:
#                                the CNI-HOSTPORT-DNAT jump rules vanish from
#                                the VM's nat PREROUTING/OUTPUT chains when
#                                k3s/kube-proxy rewrites iptables underneath
#                                already-running klipper svclb pods (VM boots,
#                                k3s restarts). Cure: bounce the svclb pods —
#                                the CNI portmap re-installs chains AND jumps.
#   Stage 5  TB / reporting?     billing-ledger readyz probe (a TB wedge is
#                                cured by the Stage-1 VM restart, nothing less)
#
# Learned the hard way (DON'T re-add these):
#   - The node registering on 192.168.5.x (eth0) is BY DESIGN: Rancher
#     Desktop pins `--node-ip <eth0> --node-external-ip <vznat>` in
#     /etc/conf.d/k3s. It is not a fault, don't "fix" it.
#   - Do NOT restart k3s inside the VM as a repair step: the iptables
#     rewrite races running hostPort pods and STRIPS the CNI-HOSTPORT jump
#     rules — that turns a tunnel problem into a worse tunnel problem.
#   - If svclb pods come up broken REPEATEDLY on one VM boot (klipper's
#     entrypoint logging a `FORWARD ... -j DROP` because it read
#     ip_forward=0 despite the daemonset's securityContext sysctl — a
#     sandbox-creation race), the VIRTUALIZATION LAYER is degraded, not
#     the cluster. Seen 2026-07-26 after laptop sleep: three svclb
#     generations in a row misbehaved across three VM boots. In-cluster
#     surgery cannot fix that boot; the escalation is a clean VM restart,
#     and if it recurs, a macOS REBOOT (post-sleep VZ framework state).
#
# Safe to run any time: every stage checks before it acts, and every action
# is a wait, an rdctl restart, or a daemonset-managed pod bounce. Never
# touches data (PVCs) or helm state.
set -uo pipefail

NS=adtech
CORE_PORTS=(8086 8080 8081 8084)   # reporting, gateway, exchange, ssp
MIN_READY_PODS=25

say()  { echo "[stack-doctor] $*"; }
fail() { echo "[stack-doctor] FAIL: $*" >&2; exit 1; }

api_up()    { kubectl get nodes >/dev/null 2>&1; }
docker_up() { docker ps >/dev/null 2>&1; }
tunnel_up() { curl -sf -o /dev/null --max-time 3 "http://localhost:$1/healthz"; }

wait_for() { # wait_for <seconds> <desc> <fn...>
  local deadline=$(( $(date +%s) + $1 )); local desc=$2; shift 2
  until "$@"; do
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 5
  done
  say "$desc: ok"
}

# ---- Stage 1: Rancher Desktop app + VM -------------------------------------
command -v rdctl >/dev/null || fail "rdctl not found — is Rancher Desktop installed?"
if ! pgrep -f 'Rancher Desktop.app' >/dev/null; then
  say "Rancher Desktop app not running — starting it"
  rdctl start >/dev/null 2>&1 || true
fi

# ---- Stage 2: k8s API -------------------------------------------------------
if ! api_up; then
  say "k8s API down — waiting up to 3m for the VM"
  if ! wait_for 180 "k8s API" api_up; then
    if ! docker_up; then
      say "app is up but the VM is wedged (post-sleep signature) — clean VM restart"
      rdctl shutdown >/dev/null 2>&1 || true
      rdctl start >/dev/null 2>&1 || true
    fi
    wait_for 300 "k8s API after restart" api_up || fail "k8s API never came up — check the Rancher Desktop GUI"
  fi
fi
say "k8s API: ok"

# ---- Stage 3: adtech pods ---------------------------------------------------
pods_ready() { [ "$(kubectl -n $NS get pods 2>/dev/null | grep -cE '1/1\s+Running|2/2\s+Running')" -ge "$MIN_READY_PODS" ]; }
wait_for 420 "adtech pods ($MIN_READY_PODS+ ready)" pods_ready \
  || fail "pods not converging — inspect: kubectl -n $NS get pods | grep -v Running"

# ---- Stage 4: localhost tunnels (hostPort-jump signature) --------------------
all_tunnels() { for p in "${CORE_PORTS[@]}"; do tunnel_up "$p" || return 1; done; }
if ! all_tunnels; then
  say "localhost tunnels dead — checking layers"
  kubectl -n $NS exec deploy/gateway -- wget -qO- --timeout=3 "http://reporting:8086/healthz" >/dev/null 2>&1 \
    || fail "in-cluster path ALSO dead — not a tunnel problem; inspect service pods"
  if ! rdctl shell sudo iptables -t nat -S PREROUTING 2>/dev/null | grep -q CNI-HOSTPORT; then
    say "CNI-HOSTPORT jump rules missing from the VM nat table (k3s rewrote iptables under the svclb pods)"
  else
    say "jump rules present but tunnels dead — svclb pods likely hold stale forwarding state"
  fi
  say "bouncing klipper svclb pods (daemonset-managed; CNI portmap reprograms chains + jumps)"
  kubectl -n kube-system delete pods -l svccontroller.k3s.cattle.io/svcname >/dev/null 2>&1
  if ! wait_for 240 "tunnels after svclb bounce" all_tunnels; then
    # Sysctl-race signature: klipper installed its ip_forward=0 DROP even
    # though the daemonset requests ip_forward=1 — the sandbox raced. One
    # pod in this state means the whole svclb generation is suspect and
    # the VM boot itself is degraded.
    pod=$(kubectl -n kube-system get pods -o name 2>/dev/null | grep svclb-reporting | head -1)
    if [ -n "$pod" ] && kubectl -n kube-system logs "${pod#pod/}" --all-containers 2>/dev/null | grep -q 'FORWARD.*-j DROP'; then
      fail "svclb pods raced ip_forward at creation (degraded VM boot). Escalate: rdctl shutdown && rdctl start, re-run; if it recurs, REBOOT macOS (post-sleep VZ framework state)."
    fi
    fail "tunnels still dead after svclb bounce — escalate to a clean VM restart (rdctl shutdown && rdctl start) and re-run; if it recurs, reboot macOS"
  fi
fi
say "tunnels: ok (${CORE_PORTS[*]})"

# ---- Stage 5: TB / reporting readiness --------------------------------------
if ! curl -sf --max-time 5 "http://localhost:8086/readyz" | grep -q '"billing-ledger":"ok"'; then
  fail "reporting /readyz unhealthy (TB wedge?) — cure is a clean VM restart: rdctl shutdown && rdctl start, then re-run"
fi
say "reporting readyz + billing ledger: ok"

say "stack healthy ✓"
