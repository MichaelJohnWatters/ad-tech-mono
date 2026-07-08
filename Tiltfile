# Tiltfile - Ad Tech Mono development orchestration
#
# Usage:
#   tilt up                        # fast mode (build + run binary) + K8s infra
#   DEV_MODE=container tilt up     # container mode (Docker builds into K8s)
#   PROFILE=lite tilt up           # lite infra (no observability)

dev_mode = os.getenv('DEV_MODE', 'fast')
profile = os.getenv('PROFILE', 'full')

# Kill orphaned processes from previous sessions by port. Skips Tilt's
# own port-forwards and their kubectl subprocesses — once services moved
# to pods, those ports are owned by Tilt itself, and a blind `kill -9`
# would terminate the running Tilt instance during a Tiltfile reload.
local('''for port in 8080 8081 8082 8083 8084 8085 8086 8087 8089 8090 8091; do
    for pid in $(lsof -ti :$port 2>/dev/null); do
        pname=$(ps -p $pid -o comm= 2>/dev/null | tr -d ' ')
        case "$pname" in
            tilt|kubectl|*tilt*) ;;
            *) kill -9 $pid 2>/dev/null ;;
        esac
    done
done
sleep 1
echo "ports cleared"
''')

# Tilt scrubs the literal value of every K8s Secret from log output by
# default. Our postgres secret has username='adtech', which collides with
# every NATS subject prefix, the Minio bucket name, the namespace, and
# half our account types. Disable scrubbing for local dev so logs stay
# readable. Staging/prod overlays should NOT do this.
secret_settings(disable_scrub=True)

# restart_process extension — modern replacement for the deprecated
# restart_container() live_update step (which is not permitted for k8s
# resources). docker_build_with_restart wraps docker_build and injects
# a supervisor that re-execs the entrypoint on live_update.
load('ext://restart_process', 'docker_build_with_restart')

# ============================================================
# Infrastructure (always runs in K8s)
# ============================================================

if profile == 'lite':
    k8s_yaml(kustomize('k8s/overlays/local-lite'))
else:
    k8s_yaml(kustomize('k8s/overlays/local'))

# Traefik ingress controller — Colima starts k3s with --disable=traefik,
# so we apply our own copy. Lives in its own namespace; kept out of the
# kustomize bundle above which pins everything to namespace: adtech.
# See k8s/base/traefik/README.md. Required for the DEV_MODE=container
# migration to route {service}.adtech.local hostnames — see
# docs/PODS_MIGRATION.md.
k8s_yaml('k8s/base/traefik/install.yaml')
k8s_resource('traefik', labels=['infra'], port_forwards=[])

k8s_resource('postgres', labels=['infra'], port_forwards=['5432:5432'])
k8s_resource('nats', labels=['infra'], port_forwards=['4222:4222', '8222:8222'])
k8s_resource('redis', labels=['infra'], port_forwards=['6379:6379'])
k8s_resource('minio', labels=['infra'], port_forwards=['9000:9000', '9001:9001'])
# ClickHouse analytics store. Native protocol forwarded to host :9010 to
# avoid colliding with Minio's :9000; HTTP UI/ping on :8123. Reporting uses
# it when reporting.analytics_backend=clickhouse (see ADR 0001).
k8s_resource('clickhouse', labels=['infra'], port_forwards=['9010:9000', '8123:8123'])
# TigerBeetle billing ledger. Native port forwarded to host :3033 (in-cluster
# 3000) so the local reporting process can use it as the durable ledger when
# BILLING_LEDGER_BACKEND=tigerbeetle (ADR 0003 part D).
k8s_resource('tigerbeetle', labels=['infra'], port_forwards=['3033:3000'])
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
k8s_resource('mailpit', labels=['infra'],
    # 1025 = SMTP (report-runner + any host process delivers here),
    # 8025 = web inbox UI. Real emails land in Mailpit locally instead of a stub log.
    port_forwards=['1025:1025', '8025:8025'],
    links=['http://localhost:8025'])

# ============================================================
# Services
# ============================================================

if dev_mode == 'fast':
    # --------------------------------------------------------
    # Fast mode: services run as K8s pods (DEV_MODE=container path).
    # Edit cmd/*/main.go → Tilt rebuilds the host binary (~1-2s) →
    # docker_build_with_restart rsyncs into the pod and re-execs
    # (sub-second). Same production-shape as staging/prod; no
    # local_resource / host-process gap. See docs/PODS_MIGRATION.md
    # for the full plan + rollback steps.
    #
    # Reporting included: its CGO parts (tigerbeetle-go) cross-compile
    # on the host via `zig cc` — see the reporting-build resource.
    # --------------------------------------------------------
    if str(local('command -v zig || true', quiet=True)).strip() == '':
        fail('zig is required (reporting CGO cross-compile fast path): brew install zig')

    # ---- Tracker (no DB deps, simplest to validate) ----
    local_resource('tracker-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/tracker ./cmd/tracker',
        deps=['cmd/tracker', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-tracker', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'tracker'},
        only=['bin/tracker', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/tracker', '/app')])
    k8s_yaml(['k8s/base/tracker/deployment.yaml', 'k8s/base/tracker/service.yaml', 'k8s/base/tracker/ingress.yaml'])
    k8s_resource('tracker', resource_deps=['tracker-build', 'nats', 'redis'],
        port_forwards=['8083:8083'], labels=['services'])

    # ---- Webhooks dispatcher (background NATS consumer, no ingress) ----
    local_resource('webhooks-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/webhooks ./cmd/webhooks',
        deps=['cmd/webhooks', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-webhooks', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'webhooks'},
        only=['bin/webhooks', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/webhooks', '/app')])
    k8s_yaml(['k8s/base/webhooks/deployment.yaml', 'k8s/base/webhooks/service.yaml'])
    k8s_resource('webhooks', resource_deps=['webhooks-build', 'nats', 'postgres'],
        port_forwards=['8091:8091'], labels=['services'])

    # ---- Identity-consumer (background NATS consumer, no ingress) ----
    local_resource('identity-consumer-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/identity-consumer ./cmd/identity-consumer',
        deps=['cmd/identity-consumer', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-identity-consumer', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'identity-consumer'},
        only=['bin/identity-consumer', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/identity-consumer', '/app')])
    k8s_yaml(['k8s/base/identity-consumer/deployment.yaml', 'k8s/base/identity-consumer/service.yaml'])
    k8s_resource('identity-consumer', resource_deps=['identity-consumer-build', 'nats', 'postgres'],
        port_forwards=['8092:8092'], labels=['services'])

    # ---- Adserver ----
    local_resource('adserver-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/adserver ./cmd/adserver',
        deps=['cmd/adserver', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-adserver', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'adserver'},
        only=['bin/adserver', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/adserver', '/app')])
    k8s_yaml(['k8s/base/adserver/deployment.yaml', 'k8s/base/adserver/service.yaml', 'k8s/base/adserver/ingress.yaml'])
    k8s_resource('adserver', resource_deps=['adserver-build', 'minio', 'redis', 'postgres'],
        port_forwards=['8085:8085'], labels=['services'])

    # ---- DSP × 3 (one image, three deployments via env-driven config) ----
    local_resource('dsp-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/dsp ./cmd/dsp',
        deps=['cmd/dsp', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-dsp', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'dsp'},
        only=['bin/dsp', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/dsp', '/app')])
    k8s_yaml(['k8s/base/dsp/deployment.yaml', 'k8s/base/dsp/service.yaml', 'k8s/base/dsp/ingress.yaml'])
    k8s_resource('dsp-internal',    resource_deps=['dsp-build', 'postgres', 'redis'], port_forwards=['8082:8082'], labels=['services'])
    k8s_resource('dsp-competitor1', resource_deps=['dsp-build', 'postgres', 'redis'], port_forwards=['8089:8089'], labels=['services'])
    k8s_resource('dsp-competitor2', resource_deps=['dsp-build', 'postgres', 'redis'], port_forwards=['8090:8090'], labels=['services'])

    # ---- SSP ----
    local_resource('ssp-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/ssp ./cmd/ssp',
        deps=['cmd/ssp', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-ssp', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'ssp'},
        only=['bin/ssp', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/ssp', '/app')])
    k8s_yaml(['k8s/base/ssp/deployment.yaml', 'k8s/base/ssp/service.yaml', 'k8s/base/ssp/ingress.yaml'])
    k8s_resource('ssp', resource_deps=['ssp-build', 'postgres'],
        port_forwards=['8084:8084'], labels=['services'])

    # ---- Exchange ----
    local_resource('exchange-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/exchange ./cmd/exchange',
        deps=['cmd/exchange', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-exchange', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'exchange'},
        only=['bin/exchange', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/exchange', '/app')])
    k8s_yaml(['k8s/base/exchange/deployment.yaml', 'k8s/base/exchange/service.yaml', 'k8s/base/exchange/ingress.yaml'])
    k8s_resource('exchange', resource_deps=['exchange-build', 'nats', 'postgres'],
        port_forwards=['8081:8081'], labels=['services'])

    # ---- Publisher-Adserver ----
    local_resource('publisher-adserver-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/publisher-adserver ./cmd/publisher-adserver',
        deps=['cmd/publisher-adserver', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-publisher-adserver', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'publisher-adserver'},
        only=['bin/publisher-adserver', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/publisher-adserver', '/app')])
    k8s_yaml(['k8s/base/publisher-adserver/deployment.yaml', 'k8s/base/publisher-adserver/service.yaml', 'k8s/base/publisher-adserver/ingress.yaml'])
    k8s_resource('publisher-adserver', resource_deps=['publisher-adserver-build', 'postgres', 'ssp', 'adserver'],
        port_forwards=['8088:8088'], labels=['services'])

    # ---- SSAI Stitcher (server-side ad insertion) ----
    local_resource('ssai-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/ssai ./cmd/ssai',
        deps=['cmd/ssai', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-ssai', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'ssai'},
        only=['bin/ssai', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/ssai', '/app')])
    k8s_yaml(['k8s/base/ssai/deployment.yaml', 'k8s/base/ssai/service.yaml'])
    k8s_resource('ssai', resource_deps=['ssai-build', 'ssp'],
        port_forwards=['8093:8093'], labels=['services'])

    # ---- Gateway (host-built binary + embedded web/ + seed binary) ----
    # The seed binary is baked into the gateway image so /dev/reset-and-reseed
    # can exec it without needing a Go toolchain inside the pod.
    local_resource('gateway-build',
        cmd='GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/gateway ./cmd/gateway && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/seed ./cmd/seed',
        deps=['cmd/gateway', 'cmd/seed', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-gateway', '.',
        dockerfile='build/Dockerfile.dev.gateway',
        only=['bin/gateway', 'bin/seed', 'web', 'profiles'],
        entrypoint='/app',
        live_update=[sync('bin/gateway', '/app'), sync('bin/seed', '/seed'), sync('web', '/web'), sync('profiles', '/profiles')])

    # ---- Gateway TLS (mkcert-issued localhost cert) ----
    # Why HTTPS at all: third-party SDKs (Google IMA, etc.) load helper
    # iframes from public origins (e.g. https://imasdk.googleapis.com).
    # Chrome's Private Network Access policy refuses fetches from those
    # iframes to loopback addresses unless the iframe is a *secure
    # context*, and Google's SDK mirrors the host page's protocol — so
    # the host page must be HTTPS for the IMA bridge iframe to inherit
    # secure-context. mkcert issues a cert from a per-user root CA that
    # the system keychain already trusts, so browsers see no warning.
    #
    # setup-tls.sh is idempotent and always exits 0 — if mkcert isn't
    # installed it prints brew instructions and skips. In that case
    # the Secret below isn't created, the gateway's `optional: true`
    # volume silently no-ops, and the HTTPS listener logs a warning and
    # skips itself (HTTP on 8080 keeps working).
    local('bash scripts/setup-tls.sh')
    tls_cert_path = 'dev/tls/localhost.pem'
    tls_key_path = 'dev/tls/localhost-key.pem'
    if os.path.exists(tls_cert_path) and os.path.exists(tls_key_path):
        # Apply the Secret via kubectl. Wrapped in a shell that returns 0
        # even if kubectl fails — otherwise a transient cluster outage
        # (Colima crashed, k3s API still booting) would abort the entire
        # Tiltfile parse, which in turn blocks `tilt up` from running the
        # recovery resources that would have brought the cluster back.
        # The gateway's volume mount is marked optional so a missing
        # Secret just means no HTTPS listener — HTTP on 8080 continues
        # to work, and a later 'tilt trigger' picks up the Secret once
        # k3s is healthy again.
        local('kubectl create secret generic gateway-tls -n adtech ' +
              '--from-file=cert.pem=' + tls_cert_path + ' ' +
              '--from-file=key.pem=' + tls_key_path + ' ' +
              '--dry-run=client -o yaml | kubectl apply -f - ' +
              '|| echo "[tilt] kubectl apply failed (k3s may be down) — gateway-tls Secret skipped"',
              quiet=True)

    k8s_yaml(['k8s/base/gateway/deployment.yaml', 'k8s/base/gateway/service.yaml', 'k8s/base/gateway/ingress.yaml'])
    k8s_resource('gateway', resource_deps=['gateway-build', 'postgres', 'redis'],
        port_forwards=['8080:8080', '8443:8443'], labels=['services'],
        links=['https://localhost:8443', 'https://localhost:8443/dev/publisher-simulator', 'http://localhost:8080', 'http://gateway.adtech.local'])

    # ---- Reporting (POD — host cross-compile via zig, same fast path as
    # every other service) ----
    # reporting imports tigerbeetle-go (CGO), which used to force an in-image
    # compile — the one heavy docker build left, and the thing that kept
    # wedging the Colima VM under buildkit load. `zig cc` cross-compiles the
    # CGO parts against musl on the HOST instead (tigerbeetle-go's static lib
    # is musl-safe), producing a static Linux ELF: first build ~30min (zig
    # populates its cache), incrementals ~2s, and the VM only ever does a
    # trivial COPY. Prereq: `brew install zig` (checked at Tiltfile load).
    # build/Dockerfile.reporting remains the prod/CI in-image build.
    local_resource('reporting-build',
        cmd='CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC="zig cc -target x86_64-linux-musl" go build -o ./bin/reporting ./cmd/reporting',
        deps=['cmd/reporting', 'pkg/'], labels=['build'])
    docker_build_with_restart('adtech-reporting', '.',
        dockerfile='build/Dockerfile.dev',
        build_args={'SERVICE': 'reporting'},
        only=['bin/reporting', 'web'],
        entrypoint='/app',
        live_update=[sync('bin/reporting', '/app')])
    k8s_yaml(['k8s/base/reporting/deployment.yaml', 'k8s/base/reporting/service.yaml'])
    k8s_resource('reporting', resource_deps=['reporting-build', 'nats', 'postgres', 'clickhouse', 'tigerbeetle'],
        port_forwards=['8086:8086'], labels=['services'])

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
    # S3_ENDPOINT points the seed at the port-forwarded Minio so it
    # can upload themed creative SVGs for the asset_url half of the
    # creative split. Without this seed logs "s3.endpoint not set"
    # and every creative falls back to inline HTML — degraded but
    # functional. SEED_CREATIVES_URL_BASE is the browser-facing prefix
    # the inserter records in creatives.html_content (it wraps
    # <img src=…>), so it must point at the gateway's /v1/creatives/*
    # reverse proxy, not at Minio directly.
    cmd='S3_ENDPOINT=localhost:9000 S3_ACCESS_KEY=adtech S3_SECRET_KEY=adtech-local-dev SEED_CREATIVES_URL_BASE=http://localhost:8080/v1/creatives go run ./cmd/seed --profile standard',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False)

local_resource('seed-via-api',
    # The API twin of seed-standard: walks the real customer onboarding
    # journey (signup → site → placement → campaign → topup → auction)
    # through the gateway with real sessions, doubling as the golden-path
    # e2e test. NOTE: resets tenant tables first for determinism — rerun
    # seed-standard afterwards if you want the demo inventory + dev logins
    # back. Requires real-auth mode (jwt_signing secret seeded + gateway
    # restarted since).
    cmd='go test -tags e2e -count=1 -run TestOnboardingJourney ./tests/e2e',
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

local_resource('adstxt-crawl',
    # ads.txt crawler (normally a daily CronJob). Runs on the host against the
    # port-forwarded postgres + nats: fetches each publisher's ads.txt into
    # ads_txt_cache and publishes the invalidate so the exchange re-reads.
    # 127.0.0.1 (not localhost) forces IPv4 so pq doesn't try the ::1 the
    # port-forward isn't listening on.
    cmd='DATABASE_URL=postgres://adtech:adtech-local-dev@127.0.0.1:5432/adtech?sslmode=disable NATS_URL=nats://127.0.0.1:4222 go run ./cmd/adstxt',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False,
    resource_deps=['postgres', 'nats'])

local_resource('appadstxt-crawl',
    # app-ads.txt crawler (in-app sibling of adstxt-crawl; normally a daily
    # CronJob). Fetches each app publisher's developer-domain app-ads.txt into
    # app_ads_txt_cache. No NATS invalidate yet — no warm-cache consumer until
    # app-ads.txt enforcement lands. 127.0.0.1 forces IPv4 (see adstxt-crawl).
    cmd='DATABASE_URL=postgres://adtech:adtech-local-dev@127.0.0.1:5432/adtech?sslmode=disable go run ./cmd/appadstxt',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False,
    resource_deps=['postgres'])

local_resource('report-runner',
    # Scheduled-report runner (normally a periodic CronJob). Runs every saved
    # report whose interval schedule (@hourly/@daily/@weekly/@monthly) is due
    # and emails the result via Mailpit (SMTP on 127.0.0.1:1025) — view deliveries
    # at http://localhost:8025. 127.0.0.1 forces IPv4 (see adstxt-crawl).
    cmd='DATABASE_URL=postgres://adtech:adtech-local-dev@127.0.0.1:5432/adtech?sslmode=disable REPORT_RUNNER_REPORTING_URL=http://127.0.0.1:8086 REPORT_RUNNER_SMTP_HOST=127.0.0.1:1025 go run ./cmd/report-runner',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False,
    resource_deps=['postgres', 'reporting', 'mailpit'])

local_resource('privacy-delete',
    # Level-3 (full deletion) runner (normally a periodic CronJob). Purges every
    # user with a pending level-3 opt_out_registry row across identity_graph +
    # audience_segment_members, marks it completed, announces the completion on
    # NATS. 127.0.0.1 forces IPv4 (see adstxt-crawl).
    cmd='DATABASE_URL=postgres://adtech:adtech-local-dev@127.0.0.1:5432/adtech?sslmode=disable PRIVACY_DELETE_NATS_URL=nats://127.0.0.1:4222 go run ./cmd/privacy-delete',
    trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'], auto_init=False,
    resource_deps=['postgres', 'nats'])

local_resource('privacy-verify',
    # Deletion auditor (normally a periodic CronJob, run after privacy-delete).
    # Re-checks completed deletions for residual rows; stamps verified_at when
    # clean, exits non-zero (ops alert) when data survived.
    cmd='DATABASE_URL=postgres://adtech:adtech-local-dev@127.0.0.1:5432/adtech?sslmode=disable go run ./cmd/privacy-verify',
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
