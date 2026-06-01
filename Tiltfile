# Tiltfile - Ad Tech Mono development orchestration
#
# Usage:
#   tilt up                        # fast mode (build + run binary) + K8s infra
#   DEV_MODE=container tilt up     # container mode (Docker builds into K8s)
#   PROFILE=lite tilt up           # lite infra (no observability)

dev_mode = os.getenv('DEV_MODE', 'fast')
profile = os.getenv('PROFILE', 'full')

# Kill orphaned processes from previous sessions by port
local('for port in 8080 8081 8082 8083 8084 8085 8086 8087 8089 8090; do lsof -ti :$port 2>/dev/null | xargs kill -9 2>/dev/null; done; sleep 1; echo "ports cleared"')

# Tilt scrubs the literal value of every K8s Secret from log output by
# default. Our postgres secret has username='adtech', which collides with
# every NATS subject prefix, the Minio bucket name, the namespace, and
# half our account types. Disable scrubbing for local dev so logs stay
# readable. Staging/prod overlays should NOT do this.
secret_settings(disable_scrub=True)

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
k8s_resource('grafana', labels=['observability'], port_forwards=['3000:3000'],
    links=['http://localhost:3000'])
k8s_resource('prometheus', labels=['observability'], port_forwards=['9090:9090'],
    links=['http://localhost:9090'])
k8s_resource('loki', labels=['observability'], port_forwards=['3100:3100'])
k8s_resource('promtail', labels=['observability'])
k8s_resource('jaeger', labels=['observability'],
    # 16686 = UI, 4317 = OTLP/gRPC, 4318 = OTLP/HTTP (what pkg/tracing uses).
    # Without 4318 forwarded, every service spams "dial tcp :4318 connection refused".
    port_forwards=['16686:16686', '4317:4317', '4318:4318'],
    links=['http://localhost:16686'])

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

    # readiness_probe makes Tilt's "ready" state reflect actual /readyz
    # success (DB + cache + bus connected), not just "process started".
    # Pods stay yellow until checks pass, so cascade-failures surface fast.
    def ready(port):
        return probe(period_secs=2, http_get=http_get_action(port=port, path='/readyz'))

    local_resource('gateway',
        cmd='go build -o ./bin/gateway ./cmd/gateway',
        serve_cmd='POD_NAME=gateway-0 LOKI_URL=http://localhost:3100 ./bin/gateway',
        serve_dir='.',
        deps=['cmd/gateway', 'pkg/', 'web/'],
        labels=['services'],
        resource_deps=['postgres', 'redis'],
        readiness_probe=ready(8080),
        links=['http://localhost:8080', 'http://localhost:8080/dev/publisher-simulator'])

    local_resource('exchange',
        cmd='go build -o ./bin/exchange ./cmd/exchange',
        serve_cmd='POD_NAME=exchange-0 LOKI_URL=http://localhost:3100 ./bin/exchange',
        serve_dir='.',
        deps=['cmd/exchange', 'pkg/'],
        labels=['services'],
        resource_deps=['nats', 'postgres'],
        readiness_probe=ready(8081))

    local_resource('dsp',
        cmd='go build -o ./bin/dsp ./cmd/dsp',
        serve_cmd='POD_NAME=dsp-internal-0 LOKI_URL=http://localhost:3100 ./bin/dsp',
        serve_dir='.',
        deps=['cmd/dsp', 'pkg/'],
        labels=['services'],
        resource_deps=['postgres', 'redis'],
        readiness_probe=ready(8082))

    local_resource('dsp-competitor1',
        cmd='go build -o ./bin/dsp ./cmd/dsp',
        serve_cmd='DSP_PORT=8089 DSP_PROFILE=competitor1 POD_NAME=dsp-competitor1 LOKI_URL=http://localhost:3100 ./bin/dsp',
        serve_dir='.',
        deps=['cmd/dsp', 'pkg/'],
        labels=['services'],
        resource_deps=['postgres', 'redis'],
        readiness_probe=ready(8089))

    local_resource('dsp-competitor2',
        cmd='go build -o ./bin/dsp ./cmd/dsp',
        serve_cmd='DSP_PORT=8090 DSP_PROFILE=competitor2 POD_NAME=dsp-competitor2 LOKI_URL=http://localhost:3100 ./bin/dsp',
        serve_dir='.',
        deps=['cmd/dsp', 'pkg/'],
        labels=['services'],
        resource_deps=['postgres', 'redis'],
        readiness_probe=ready(8090))

    local_resource('tracker',
        cmd='go build -o ./bin/tracker ./cmd/tracker',
        serve_cmd='POD_NAME=tracker-0 LOKI_URL=http://localhost:3100 ./bin/tracker',
        serve_dir='.',
        deps=['cmd/tracker', 'pkg/'],
        labels=['services'],
        resource_deps=['nats', 'redis'],
        readiness_probe=ready(8083))

    local_resource('ssp',
        cmd='go build -o ./bin/ssp ./cmd/ssp',
        serve_cmd='POD_NAME=ssp-0 LOKI_URL=http://localhost:3100 ./bin/ssp',
        serve_dir='.',
        deps=['cmd/ssp', 'pkg/'],
        labels=['services'],
        resource_deps=['postgres'],
        readiness_probe=ready(8084))

    local_resource('adserver',
        cmd='go build -o ./bin/adserver ./cmd/adserver',
        serve_cmd='POD_NAME=adserver-0 LOKI_URL=http://localhost:3100 ./bin/adserver',
        serve_dir='.',
        deps=['cmd/adserver', 'pkg/'],
        labels=['services'],
        resource_deps=['minio', 'redis', 'postgres'],
        readiness_probe=ready(8085))

    local_resource('reporting',
        cmd='go build -o ./bin/reporting ./cmd/reporting',
        serve_cmd='POD_NAME=reporting-0 LOKI_URL=http://localhost:3100 ./bin/reporting',
        serve_dir='.',
        deps=['cmd/reporting', 'pkg/'],
        labels=['services'],
        resource_deps=['nats', 'postgres'],
        readiness_probe=ready(8086))

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

local_resource('day-boundary',
    cmd='go run ./cmd/dayboundary',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False)

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
    cmd='go test ./pkg/... ./cmd/...',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['tests'], auto_init=False)

# E2E tests run against the FULL deployment — every service + every infra
# dep must be healthy first. A partial stack defeats the point of e2e.
# Reflected in resource_deps below.
e2e_full_deps = [
    'postgres', 'nats', 'redis', 'minio',
    'gateway', 'exchange', 'dsp', 'dsp-competitor1', 'dsp-competitor2',
    'ssp', 'adserver', 'tracker', 'reporting',
]

# Full e2e suite: every Test* in tests/e2e/.
# Resets DB and Redis at the start of each Test* (see harness.Reset), so
# don't run while a simulator is actively pushing traffic.
local_resource('test-e2e',
    cmd='go test ./tests/... -tags=e2e -count=1 -timeout=10m -v',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['tests'], auto_init=False,
    resource_deps=e2e_full_deps)

# Individual test buttons for fast iteration on one scenario.
# Same full-deployment requirement — partial stack = invalid test.
local_resource('e2e-narrative',
    cmd='go test ./tests/e2e/... -tags=e2e -run TestEndToEnd -count=1 -timeout=5m -v',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['tests'], auto_init=False,
    resource_deps=e2e_full_deps)

local_resource('e2e-rls',
    cmd='go test ./tests/e2e/... -tags=e2e -run TestRLSIsolation -count=1 -timeout=2m -v',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['tests'], auto_init=False,
    resource_deps=e2e_full_deps)

local_resource('e2e-budget',
    cmd='go test ./tests/e2e/... -tags=e2e -run TestBudgetCap -count=1 -timeout=3m -v',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['tests'], auto_init=False,
    resource_deps=e2e_full_deps)

# Legacy shell smoke test — kept for now in case anyone scripts against it.
local_resource('test-e2e-shell-legacy',
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

  REPORTING (:8086) - Analytics & event ingestion
  ─────────────────────────────────────────────────────────
  POST /v1/reporting/query               Query analytics store
  POST /v1/reporting/events              HTTP event ingestion (standalone)

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

  E2E TESTS (Tilt buttons or CLI)
  ─────────────────────────────────────────────────────────
  All:              make test-e2e            (or click test-e2e in Tilt)
  Narrative:        click e2e-narrative      (full TestEndToEnd compounding)
  RLS only:         click e2e-rls            (multi-tenant isolation)
  Budget cap:       click e2e-budget         (daily_budget enforcement)
  Refresh caches:   curl -XPOST localhost:8082/debug/cache/refresh

============================================================
'""",
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['info'], auto_init=False)
