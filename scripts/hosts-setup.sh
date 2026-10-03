#!/usr/bin/env bash
#
# hosts-setup.sh — point every *.adtech.local ingress hostname (core services +
# the in-cluster demo publisher sites) at the local Traefik ingress, so a browser
# can reach the HTTPS stack by name (https://gateway.adtech.local,
# https://twitchr.adtech.local, …).
#
# The hostnames are DERIVED from the Helm chart's rendered Ingresses, so this
# never drifts when a service or demo site is added/removed. Writes ONE managed
# block into /etc/hosts (idempotent — re-running rewrites just that block).
#
#   make hosts                               # auto: the Traefik LB IP (instant)
#   ADTECH_HOSTS_IP=127.0.0.1 make hosts      # force loopback (has the IPv6 lag)
#
# DEFAULT = the Traefik LoadBalancer's real IPv4 IP, NOT 127.0.0.1. A loopback
# name makes macOS synthesize an IPv4-mapped IPv6 address (::ffff:127.0.0.1) that
# the browser's Happy-Eyeballs tries FIRST — and since the host :443 tunnel is
# IPv4-only, that IPv6 attempt hangs ~5s per first-connect before falling back.
# The node IP is plain IPv4 (no synthesis) and reachable directly (no tunnel), so
# loads are instant. Re-run after an RD restart if the node IP changes. Editing
# /etc/hosts needs sudo; if unavailable the script prints the block to paste.
set -euo pipefail
cd "$(dirname "$0")/.."

# Auto-detect the Traefik LoadBalancer IP; fall back to loopback if unavailable.
IP="${ADTECH_HOSTS_IP:-$(kubectl get svc traefik -n traefik -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)}"
IP="${IP:-127.0.0.1}"
CHART="k8s/helm/adtech"
HOSTS_FILE="${ADTECH_HOSTS_FILE:-/etc/hosts}"
BEGIN="# BEGIN adtech-hosts — managed by scripts/hosts-setup.sh (make hosts)"
END="# END adtech-hosts"

command -v helm >/dev/null 2>&1 || { echo "✗ helm not found on PATH"; exit 1; }

# Derive hostnames from BOTH the core chart and the (default-off) demosites
# template, dedupe, stable order. (newline-separated string — portable to the
# bash 3.2 that ships on macOS; hostnames have no spaces/glob chars.)
echo "▶ deriving *.adtech.local hostnames from $CHART …"
HOSTS="$(
  {
    helm template adtech "$CHART" --set tls.enabled=true 2>/dev/null
    helm template adtech "$CHART" --set global.demosites=true --set tls.enabled=true \
      --set-string demosites.tls.crt=eHg= --set-string demosites.tls.key=eHg= \
      --show-only templates/demosites.yaml 2>/dev/null
    helm template adtech "$CHART" --set global.demosites=true --set tls.enabled=true \
      --set-string demostore.tls.crt=eHg= --set-string demostore.tls.key=eHg= \
      --show-only templates/demostore.yaml 2>/dev/null
  } | grep -Eo 'host: ([a-z0-9.-]+\.)?adtech\.local' | awk '{print $2}' | sort -u
)"
[ -n "$HOSTS" ] || { echo "✗ no *.adtech.local hosts rendered from the chart"; exit 1; }
echo "  ✔ $(printf '%s\n' "$HOSTS" | wc -l | tr -d ' ') hostnames → $IP"

# Build the managed block.
block="$BEGIN"$'\n'
for h in $HOSTS; do block+="$IP $h"$'\n'; done
block+="$END"

# Compose the target file: strip any old managed block (+ trailing blank lines),
# append the fresh one. Then no-op if it's already identical.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
{
  # strip any prior managed block (BEGIN..END inclusive), then drop trailing
  # blank lines — all in awk (portable; avoids BSD-sed label quirks).
  awk '/^# BEGIN adtech-hosts/{skip=1} skip==0{print} /^# END adtech-hosts$/{skip=0}' "$HOSTS_FILE" \
    | awk 'NF{last=NR} {line[NR]=$0} END{for(i=1;i<=last;i++)print line[i]}'
  printf '\n%s\n' "$block"
} > "$tmp"

if cmp -s "$tmp" "$HOSTS_FILE"; then
  echo "✔ $HOSTS_FILE already up to date — nothing to do."
  exit 0
fi

echo "▶ updating $HOSTS_FILE (managed block):"
printf '%s\n' "$block" | sed 's/^/    /'

# Write it. /etc/hosts needs root; back it up first.
write() { cp "$HOSTS_FILE" "${HOSTS_FILE}.adtech.bak" 2>/dev/null || true; cat "$tmp" > "$HOSTS_FILE"; }
if [ -w "$HOSTS_FILE" ]; then
  write
elif command -v sudo >/dev/null 2>&1; then
  echo "  … writing via sudo (you may be prompted)"
  sudo cp "$HOSTS_FILE" "${HOSTS_FILE}.adtech.bak" 2>/dev/null || true
  sudo tee "$HOSTS_FILE" < "$tmp" >/dev/null
else
  echo "✗ can't write $HOSTS_FILE (no write perm, no sudo). Paste the block above manually."
  exit 1
fi

echo "✔ $HOSTS_FILE updated (backup: ${HOSTS_FILE}.adtech.bak)."
echo "  browse: https://gateway.adtech.local · https://twitchr.adtech.local · https://soundwave.adtech.local"
