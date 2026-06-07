#!/usr/bin/env bash
# Generate a locally-trusted TLS cert for the dev gateway via mkcert.
#
# Why we need this:
#   The IMA SDK (and other third-party SDKs) load helper iframes
#   from public origins like https://imasdk.googleapis.com. Those
#   iframes then fetch resources from our gateway. As of Chrome 117,
#   such fetches into a private/loopback address (localhost) are
#   refused outright unless the iframe is in a *secure context*, and
#   Google's SDK mirrors the host page's protocol — so the only way
#   to get a secure-context iframe is to load the host page over HTTPS.
#
# Why mkcert rather than a self-signed cert:
#   mkcert installs a per-user root CA into the system keychain and
#   issues localhost certs from it. Chrome trusts them without any
#   "Not Secure" warning. Self-signed certs work too but require
#   clicking through a warning every fresh profile, which is friction
#   that adds up.
#
# Idempotent: re-running is cheap. If a cert exists and is older than
# 80 days we regenerate proactively (mkcert defaults to ~825 days but
# we keep the rotation tight so the dev box catches any expiry bugs
# long before prod ever could).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CERT_DIR="${REPO_ROOT}/dev/tls"
CERT_FILE="${CERT_DIR}/localhost.pem"
KEY_FILE="${CERT_DIR}/localhost-key.pem"

if ! command -v mkcert >/dev/null 2>&1; then
  cat >&2 <<EOF
[setup-tls] mkcert is not installed — skipping cert generation.

The gateway will still boot, but only on HTTP (port 8080). To enable
HTTPS on port 8443 (needed for IMA video and other secure-context
features) install mkcert once, then re-run 'tilt up':

  brew install mkcert nss      # macOS (nss is required so the cert is
                               # trusted by Firefox as well as Chrome)
  mkcert -install              # installs the root CA into your system
                               # keychain — you may be prompted for
                               # your password

EOF
  # Exit 0 so Tilt parse / local_resource doesn't bomb. The TLS
  # listener will be absent but the rest of the stack is fine.
  exit 0
fi

mkdir -p "$CERT_DIR"

# Regenerate if the cert is missing or older than 80 days.
NEEDS_GEN=0
if [[ ! -f "$CERT_FILE" || ! -f "$KEY_FILE" ]]; then
  NEEDS_GEN=1
elif [[ -n "$(find "$CERT_FILE" -mtime +80 -print 2>/dev/null)" ]]; then
  echo "[setup-tls] cert is older than 80 days, regenerating"
  NEEDS_GEN=1
fi

if [[ "$NEEDS_GEN" == 1 ]]; then
  echo "[setup-tls] running mkcert -install (idempotent)"
  mkcert -install

  echo "[setup-tls] generating cert for localhost + 127.0.0.1"
  # gateway.adtech.local is the same hostname the existing Traefik
  # ingress advertises, so future work that switches to an ingress
  # path doesn't need a second cert.
  mkcert \
    -cert-file "$CERT_FILE" \
    -key-file "$KEY_FILE" \
    localhost 127.0.0.1 ::1 gateway.adtech.local
  echo "[setup-tls] wrote $CERT_FILE + $KEY_FILE"
else
  echo "[setup-tls] cert exists + recent, skipping"
fi
