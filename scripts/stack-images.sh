#!/usr/bin/env bash
# stack-images.sh — build the adtech-* service images for the Helm dev loop.
#
# The same recipe the Tiltfile used, without Tilt: cross-compile each
# service binary on the host (fast, cached), then bake it into the tiny
# per-service image. Three deviations from the generic pattern, same as
# the Tiltfile's: gateway (bakes seed + web + profiles), transcoder
# (ffmpeg base), reporting (in-image Alpine CGO build — for tigerbeetle-go;
# no longer go-duckdb since ADR 0006 phase 3 moved cold reads to ClickHouse
# s3()). migrate uses the prod in-image Dockerfile (no host binary needed).
#
# Usage:
#   scripts/stack-images.sh              # all images
#   scripts/stack-images.sh pipeline …   # just these (dsp covers all 3 DSP pods)
#
# DOCKER_CONTEXT=rancher-desktop scripts/stack-images.sh …  to target RD's
# engine explicitly.
set -euo pipefail
cd "$(dirname "$0")/.."

GOFLAGS_BUILD=(env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build)

# Generic services: host build + Dockerfile.dev.
GENERIC=(dsp ssp exchange adserver publisher-adserver tracker pipeline webhooks notifications identity-consumer audience-rt report-runner ssai batch-conductor dayboundary invoice-runner account-closeout)

build_generic() {
  local svc=$1
  "${GOFLAGS_BUILD[@]}" -o "./bin/$svc" "./cmd/$svc"
  docker build -q --build-arg "SERVICE=$svc" -f build/Dockerfile.dev -t "adtech-$svc" . >/dev/null
  echo "built adtech-$svc"
}

build_one() {
  case $1 in
    gateway)
      "${GOFLAGS_BUILD[@]}" -o ./bin/gateway ./cmd/gateway
      "${GOFLAGS_BUILD[@]}" -o ./bin/seed ./cmd/seed
      docker build -q -f build/Dockerfile.dev.gateway -t adtech-gateway . >/dev/null
      echo "built adtech-gateway" ;;
    transcoder)
      "${GOFLAGS_BUILD[@]}" -o ./bin/transcoder ./cmd/transcoder
      docker build -q --build-arg SERVICE=transcoder -f build/Dockerfile.transcode -t adtech-transcoder . >/dev/null
      echo "built adtech-transcoder" ;;
    reporting)
      # ADR 0006 phase 3: cold reads go through ClickHouse s3() (pure Go), so
      # reporting no longer needs the duckdb tag / glibc go-duckdb build. Standard
      # Alpine image (CGO only for tigerbeetle-go) — much faster to build.
      docker build -q -f build/Dockerfile.reporting -t adtech-reporting . >/dev/null
      echo "built adtech-reporting" ;;
    migrate)
      "${GOFLAGS_BUILD[@]}" -o ./bin/migrate ./cmd/migrate
      docker build -q -f build/Dockerfile.migrate -t adtech-migrate . >/dev/null
      echo "built adtech-migrate" ;;
    *)
      build_generic "$1" ;;
  esac
}

if [ $# -gt 0 ]; then
  for svc in "$@"; do build_one "$svc"; done
  exit 0
fi

for svc in "${GENERIC[@]}"; do build_one "$svc"; done
build_one gateway
build_one transcoder
build_one reporting
build_one migrate
echo "all images built"
