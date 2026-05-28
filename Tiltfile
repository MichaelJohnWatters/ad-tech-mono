# Tiltfile - Ad Tech Mono development orchestration
#
# Usage:
#   tilt up                        # fast mode (build + run binary) + K8s infra
#   DEV_MODE=container tilt up     # container mode (Docker builds into K8s)
#   PROFILE=lite tilt up           # lite infra (no observability)

dev_mode = os.getenv('DEV_MODE', 'fast')
profile = os.getenv('PROFILE', 'full')

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
