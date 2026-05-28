# Tiltfile - Ad Tech Mono development orchestration
#
# Usage:
#   tilt up                        # fast mode (build + run binary) + K8s infra
#   DEV_MODE=container tilt up     # container mode (Docker builds into K8s)
#   PROFILE=lite tilt up           # lite infra (no observability)

dev_mode = os.getenv('DEV_MODE', 'fast')
profile = os.getenv('PROFILE', 'full')

# Kill orphaned processes from previous sessions by port
local('for port in 8080 8081 8082 8083 8084 8085; do lsof -ti :$port 2>/dev/null | xargs kill -9 2>/dev/null; done; sleep 1; echo "ports cleared"')

# ============================================================
# Infrastructure (always runs in K8s)
# ============================================================

if profile == 'lite':
    k8s_yaml(kustomize('k8s/overlays/local-lite'))
else:
    k8s_yaml(kustomize('k8s/overlays/local'))

k8s_resource('postgres', labels=['infra'], port_forwards=['5432:5432'])
k8s_resource('nats', labels=['infra'], port_forwards=['4222:4222', '8222:8222'])
k8s_resource('redis', labels=['infra'], port_forwards=['6379:6379'])
k8s_resource('minio', labels=['infra'], port_forwards=['9000:9000', '9001:9001'])

# ============================================================
# Services
# ============================================================

if dev_mode == 'fast':
    # --------------------------------------------------------
    # Fast mode: build binary + run it. Tilt watches for file
    # changes, rebuilds the binary (~1-2s), and restarts cleanly.
    # Unlike `go run`, running a binary directly means Tilt can
    # kill the process cleanly (no orphaned child processes).
    # --------------------------------------------------------

    local_resource('gateway',
        cmd='go build -o ./bin/gateway ./cmd/gateway',
        serve_cmd='./bin/gateway',
        serve_dir='.',
        deps=['cmd/gateway', 'pkg/', 'web/'],
        labels=['services'],
        resource_deps=['postgres', 'redis'],
        links=['http://localhost:8080', 'http://localhost:8080/dev/publisher-simulator'])

    local_resource('exchange',
        cmd='go build -o ./bin/exchange ./cmd/exchange',
        serve_cmd='./bin/exchange',
        serve_dir='.',
        deps=['cmd/exchange', 'pkg/'],
        labels=['services'],
        resource_deps=['nats'])

    local_resource('dsp',
        cmd='go build -o ./bin/dsp ./cmd/dsp',
        serve_cmd='./bin/dsp',
        serve_dir='.',
        deps=['cmd/dsp', 'pkg/'],
        labels=['services'],
        resource_deps=['postgres', 'redis'])

    local_resource('tracker',
        cmd='go build -o ./bin/tracker ./cmd/tracker',
        serve_cmd='./bin/tracker',
        serve_dir='.',
        deps=['cmd/tracker', 'pkg/'],
        labels=['services'],
        resource_deps=['nats', 'redis'])

    local_resource('ssp',
        cmd='go build -o ./bin/ssp ./cmd/ssp',
        serve_cmd='./bin/ssp',
        serve_dir='.',
        deps=['cmd/ssp', 'pkg/'],
        labels=['services'],
        resource_deps=['postgres'])

    local_resource('adserver',
        cmd='go build -o ./bin/adserver ./cmd/adserver',
        serve_cmd='./bin/adserver',
        serve_dir='.',
        deps=['cmd/adserver', 'pkg/'],
        labels=['services'],
        resource_deps=['minio', 'redis'])

else:
    # --------------------------------------------------------
    # Container mode: Docker builds deployed to K8s
    # Same as CI/staging/prod. Slower but tests K8s behaviour.
    # --------------------------------------------------------

    services = ['dsp', 'ssp', 'adserver', 'tracker']
    for svc in services:
        docker_build(
            'adtech-' + svc, '.',
            dockerfile='build/Dockerfile',
            build_args={'SERVICE': svc},
            only=['cmd/' + svc, 'pkg/', 'go.mod', 'go.sum'])

    docker_build(
        'adtech-exchange', '.',
        dockerfile='build/Dockerfile',
        build_args={'SERVICE': 'exchange'},
        only=['cmd/exchange', 'pkg/', 'go.mod', 'go.sum'])

    docker_build(
        'adtech-gateway', '.',
        dockerfile='build/Dockerfile.gateway',
        only=['cmd/gateway', 'pkg/', 'web/', 'go.mod', 'go.sum'])

# ============================================================
# Seed Data
# ============================================================

local_resource('seed-minimal',
    cmd='go run ./cmd/seed --profile minimal',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False)

local_resource('seed-standard',
    cmd='go run ./cmd/seed --profile standard',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False)

local_resource('migrate',
    cmd='go run ./cmd/migrate',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False,
    resource_deps=['postgres'])

local_resource('reset',
    cmd='go run ./cmd/migrate reset && go run ./cmd/migrate && go run ./cmd/seed --profile standard',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False,
    resource_deps=['postgres'])

# ============================================================
# Simulation
# ============================================================

local_resource('sim-single',
    cmd='go run ./cmd/simulator single --geo GBR --device mobile',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['simulation'], auto_init=False,
    resource_deps=['exchange', 'dsp', 'tracker'])

local_resource('sim-trickle',
    cmd='go run ./cmd/simulator run --profile trickle --duration 2m',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['simulation'], auto_init=False,
    resource_deps=['exchange', 'dsp', 'tracker'])

local_resource('sim-steady',
    cmd='go run ./cmd/simulator run --profile steady --duration 5m',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['simulation'], auto_init=False,
    resource_deps=['exchange', 'dsp', 'tracker'])

local_resource('sim-burst',
    cmd='go run ./cmd/simulator run --profile burst --duration 1m',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['simulation'], auto_init=False,
    resource_deps=['exchange', 'dsp', 'tracker'])

# ============================================================
# Chaos Testing
# ============================================================

local_resource('chaos-kill-redis',
    cmd='kubectl -n adtech delete pod -l app=redis --force 2>/dev/null || echo "Redis not running"',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['chaos'], auto_init=False)

local_resource('chaos-kill-nats',
    cmd='kubectl -n adtech delete pod nats-0 --force 2>/dev/null || echo "NATS not running"',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['chaos'], auto_init=False)

# ============================================================
# Tests
# ============================================================

local_resource('test-unit',
    cmd='go test ./pkg/...',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['tests'], auto_init=False)

local_resource('test-e2e',
    cmd='./tests/e2e_smoke_test.sh',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['tests'], auto_init=False)

# ============================================================
# Quick Reference
# ============================================================

local_resource('endpoints',
    cmd="""echo '
============================================================
  AD TECH PLATFORM - ENDPOINT REFERENCE
============================================================

  DASHBOARDS & UI
  ─────────────────────────────────────────────────────────
  Dashboard            http://localhost:8080
  Publisher Simulator   http://localhost:8080/dev/publisher-simulator
  Tilt Dashboard       http://localhost:10350
  NATS Monitoring      http://localhost:8222
  Minio Console        http://localhost:9001  (adtech / adtech-local-dev)

  GATEWAY (:8080) - API entry point
  ─────────────────────────────────────────────────────────
  GET  /healthz                          Liveness probe
  GET  /readyz                           Readiness probe
  GET  /dev/publisher-simulator          Publisher simulator with debug overlay

  EXCHANGE (:8081) - Auctions
  ─────────────────────────────────────────────────────────
  POST /v1/openrtb/auction               Submit bid request, run auction
  GET  /v1/openrtb/win?price=&bid_id=    Win notice to DSP
  GET  /v1/openrtb/loss?bid_id=&reason=  Loss notice to DSP

  DSP (:8082) - Bidding
  ─────────────────────────────────────────────────────────
  POST /v1/openrtb/bid                   Evaluate bid request, return bid

  TRACKER (:8083) - Event pixels (browser-facing)
  ─────────────────────────────────────────────────────────
  GET  /v1/t/imp?tid=&cid=&pid=&sig=     Impression pixel (1x1 GIF)
  GET  /v1/t/click?tid=&redir=&sig=      Click redirect (302)
  GET  /v1/t/conv?tid=&type=&sig=        Conversion pixel (1x1 GIF)
  GET  /v1/t/view?tid=&dur=&pct=         Viewability beacon (204)
  GET  /v1/t/video?tid=&event=           Video event (204)
  GET  /v1/t/audio?tid=&event=           Audio event (204)

  SSP (:8084) - Publisher inventory
  ─────────────────────────────────────────────────────────
  GET  /healthz                          Liveness (gRPC services coming)

  AD SERVER (:8085) - Creative serving
  ─────────────────────────────────────────────────────────
  GET  /healthz                          Liveness (gRPC services coming)

  INFRA (K8s pods)
  ─────────────────────────────────────────────────────────
  Postgres             localhost:5432     (adtech / adtech-local-dev / adtech)
  NATS JetStream       localhost:4222     (3-node cluster)
  Redis                localhost:6379
  Minio S3             localhost:9000     (adtech / adtech-local-dev)

  QUICK TEST
  ─────────────────────────────────────────────────────────
  Single auction:   go run ./cmd/simulator single --geo GBR --device mobile
  Trickle sim:      go run ./cmd/simulator run --profile trickle --requests 5
  Steady sim:       go run ./cmd/simulator run --profile steady --duration 1m
  List profiles:    go run ./cmd/simulator profiles
  Health check:     curl http://localhost:8080/healthz

============================================================
'""",
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['info'], auto_init=False)
