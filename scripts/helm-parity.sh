#!/usr/bin/env bash
# helm-parity.sh — proves the helm chart (k8s/helm/adtech) renders the same
# stack that kustomize + Tilt deploy locally.
#
# LEFT side (source of truth) = what actually gets applied to the local
# cluster: `kubectl kustomize k8s/overlays/local` (infra + observability)
# PLUS every raw manifest the Tiltfile k8s_yaml()s directly (traefik, all app
# services, cronjobs). The app services are NOT in the kustomize base — the
# Tiltfile applies them file-by-file — so diffing against the overlay alone
# would miss the entire serving path.
#
# RIGHT side = `helm template adtech k8s/helm/adtech` (default values ==
# local). --no-hooks excludes the chart-only migrate hook Job, which has no
# kustomize/Tilt counterpart (Tilt runs cmd/migrate on the host).
#
# Both sides are normalized (resources keyed by kind/namespace/name; helm/
# kubectl-managed labels+annotations, checksum annotations, and null fields
# stripped; env and ports sorted; defaults filled) and diffed field-by-field
# by scripts/helmparity (Go — the repo already vendors gopkg.in/yaml.v3;
# python3 here has no PyYAML). Exits non-zero on any material diff.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---- LEFT: kustomize overlay + Tiltfile-applied raw manifests ----
if kubectl kustomize k8s/overlays/local > "$TMP/kustomize.yaml" 2>/dev/null; then
  :
elif command -v kustomize >/dev/null 2>&1; then
  kustomize build k8s/overlays/local > "$TMP/kustomize.yaml"
else
  echo "ERROR: neither 'kubectl kustomize' nor 'kustomize' is available" >&2
  exit 2
fi

# Keep this list in lockstep with the k8s_yaml() calls in the Tiltfile.
TILT_YAMLS=(
  k8s/base/traefik/install.yaml
  k8s/base/tracker/deployment.yaml    k8s/base/tracker/service.yaml    k8s/base/tracker/ingress.yaml
  k8s/base/webhooks/deployment.yaml   k8s/base/webhooks/service.yaml
  k8s/base/identity-consumer/deployment.yaml k8s/base/identity-consumer/service.yaml
  k8s/base/report-runner/deployment.yaml     k8s/base/report-runner/service.yaml
  k8s/base/pipeline/deployment.yaml   k8s/base/pipeline/service.yaml
  k8s/base/adserver/deployment.yaml   k8s/base/adserver/service.yaml   k8s/base/adserver/ingress.yaml
  k8s/base/dsp/deployment.yaml        k8s/base/dsp/service.yaml        k8s/base/dsp/ingress.yaml
  k8s/base/ssp/deployment.yaml        k8s/base/ssp/service.yaml        k8s/base/ssp/ingress.yaml
  k8s/base/exchange/deployment.yaml   k8s/base/exchange/service.yaml   k8s/base/exchange/ingress.yaml
  k8s/base/publisher-adserver/deployment.yaml k8s/base/publisher-adserver/service.yaml k8s/base/publisher-adserver/ingress.yaml
  k8s/base/ssai/deployment.yaml       k8s/base/ssai/service.yaml
  k8s/base/transcoder/deployment.yaml k8s/base/transcoder/service.yaml
  k8s/base/gateway/deployment.yaml    k8s/base/gateway/service.yaml    k8s/base/gateway/ingress.yaml
  k8s/base/reporting/deployment.yaml  k8s/base/reporting/service.yaml
  k8s/cronjobs/batch-conductor/cronjob.yaml
  k8s/cronjobs/dayboundary/cronjob.yaml
)
for f in "${TILT_YAMLS[@]}"; do
  printf '\n---\n' >> "$TMP/kustomize.yaml"
  cat "$f" >> "$TMP/kustomize.yaml"
done
# NOTE: reporting/ingress.yaml exists in k8s/base but the Tiltfile does NOT
# apply it (only deployment+service). The chart follows the Tiltfile.

# ---- RIGHT: helm template (default values = local) ----
if ! command -v helm >/dev/null 2>&1; then
  echo "ERROR: helm not found (brew install helm)" >&2
  exit 2
fi
helm template adtech k8s/helm/adtech --no-hooks > "$TMP/helm.yaml"

# ---- Normalize + diff ----
go run ./scripts/helmparity "$TMP/kustomize.yaml" "$TMP/helm.yaml"
