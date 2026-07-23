#!/usr/bin/env bash
# gen-dev-tls.sh — (re)generate the local dev TLS cert used by the HTTPS ingress.
#
# Produces dev/tls/localhost.pem + localhost-key.pem, a wildcard cert for the
# *.adtech.local browser-facing hosts, signed by the mkcert local CA (so your
# browser trusts it with no warning once `mkcert -install` has been run once).
# `make stack-up` loads these into the `adtech-tls` (ingress) + `gateway-tls`
# secrets. Prod uses cert-manager/Let's Encrypt instead (see values-prod.yaml).
set -euo pipefail
cd "$(dirname "$0")/.."

command -v mkcert >/dev/null || { echo "mkcert not found — brew install mkcert (then: mkcert -install)"; exit 1; }
mkdir -p dev/tls

mkcert -cert-file dev/tls/localhost.pem -key-file dev/tls/localhost-key.pem \
  "*.adtech.local" adtech.local localhost 127.0.0.1 ::1

echo "wrote dev/tls/localhost.pem (SANs: *.adtech.local, adtech.local, localhost, 127.0.0.1)"
echo
echo "For a real browser on this host, map the ingress hosts to localhost in /etc/hosts:"
echo "  127.0.0.1 gateway.adtech.local adserver.adtech.local tracker.adtech.local ssp.adtech.local pubad.adtech.local adtech.local"
echo "Then: make stack-up  (loads the cert into the adtech-tls secret)"
