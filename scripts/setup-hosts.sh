#!/bin/bash
# scripts/setup-hosts.sh — add /etc/hosts entries for the friendly
# adtech.local hostnames that k3s Traefik will route to once the
# services are migrated to pods.
#
# Idempotent: re-running just re-checks the entries; existing ones are
# left alone. Requires sudo (writing to /etc/hosts is a privileged op).
#
# Usage:
#   bash scripts/setup-hosts.sh           # adds entries (prompts for sudo)
#   bash scripts/setup-hosts.sh --remove  # removes the block
#
# After running, services migrated to pods are reachable via:
#   curl http://gateway.adtech.local
#   curl http://dsp.adtech.local/v1/dsp/campaigns
#   etc.
#
# Services still on the local_resource path (not yet podified) remain
# reachable only on localhost:<port>. This is intentional during the
# migration window — see docs/PODS_MIGRATION.md.

set -uo pipefail

MARKER_BEGIN="# BEGIN adtech-mono hosts"
MARKER_END="# END adtech-mono hosts"

# Detect Traefik's LoadBalancer IP — that's the address adtech.local
# hostnames should resolve to so requests hit the k3s ingress. Falls
# back to Colima's well-known default (192.168.5.1) if the cluster
# isn't reachable from this shell.
TRAEFIK_IP=$(kubectl --context colima -n traefik get svc traefik \
    -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
if [ -z "$TRAEFIK_IP" ]; then
    TRAEFIK_IP="192.168.5.1"
    echo "warn: couldn't read Traefik IP from cluster; defaulting to $TRAEFIK_IP"
fi
echo "Using Traefik IP: $TRAEFIK_IP"

# All hostnames the migration plan introduces. Listed even if the
# corresponding service hasn't been podified yet — adding the /etc/hosts
# entry early is harmless (it just won't resolve to anything live until
# the Ingress exists), and saves a sudo prompt later.
HOSTS=(
    gateway.adtech.local
    adtech.local
    exchange.adtech.local
    dsp.adtech.local
    dsp-int.adtech.local
    dsp-comp1.adtech.local
    dsp-comp2.adtech.local
    tracker.adtech.local
    ssp.adtech.local
    adserver.adtech.local
    reporting.adtech.local
    pipeline.adtech.local
    pubad.adtech.local
    grafana.adtech.local
    prometheus.adtech.local
    jaeger.adtech.local
    tilt.adtech.local
    minio.adtech.local
)

if [ "${1:-}" = "--remove" ]; then
    echo "Removing adtech-mono /etc/hosts block (requires sudo)…"
    sudo sed -i '' "/${MARKER_BEGIN}/,/${MARKER_END}/d" /etc/hosts
    echo "✓ removed"
    exit 0
fi

# Already present? Compare the existing block to what we'd add.
if grep -q "${MARKER_BEGIN}" /etc/hosts 2>/dev/null; then
    echo "/etc/hosts already has an adtech-mono block — leaving it alone."
    echo "  To regenerate: bash $0 --remove && bash $0"
    exit 0
fi

# Build the new block as a temp file so we can show it to the user
# before touching /etc/hosts.
tmp=$(mktemp)
{
    echo ""
    echo "${MARKER_BEGIN}"
    echo "# Added by scripts/setup-hosts.sh — local k3s Traefik routes."
    echo "# Remove with: bash scripts/setup-hosts.sh --remove"
    for h in "${HOSTS[@]}"; do
        printf '%s\t%s\n' "$TRAEFIK_IP" "$h"
    done
    echo "${MARKER_END}"
} > "$tmp"

echo "About to append the following to /etc/hosts (sudo required):"
echo "----"
cat "$tmp"
echo "----"
read -p "Proceed? [y/N] " ans
if [ "$ans" != "y" ] && [ "$ans" != "Y" ]; then
    echo "aborted"
    rm "$tmp"
    exit 1
fi

sudo sh -c "cat '$tmp' >> /etc/hosts"
rm "$tmp"
# DNS cache flush so the new entries take effect immediately.
sudo dscacheutil -flushcache 2>/dev/null || true
sudo killall -HUP mDNSResponder 2>/dev/null || true
echo "✓ /etc/hosts updated; DNS cache flushed"
