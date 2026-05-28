# Tiltfile - Ad Tech Mono development orchestration
#
# Usage:
#   tilt up                    # default: fast mode (go run) + full infra
#   DEV_MODE=container tilt up # container mode (Docker builds)
#   PROFILE=lite tilt up       # lite mode (no observability stack)

# --- Configuration ---

dev_mode = os.getenv('DEV_MODE', 'fast')  # 'fast' (go run) or 'container' (Docker)
profile = os.getenv('PROFILE', 'full')     # 'full' or 'lite'

# --- Infrastructure (always runs in K8s) ---

if profile == 'lite':
    k8s_yaml(kustomize('k8s/overlays/local-lite'))
else:
    k8s_yaml(kustomize('k8s/overlays/local'))

# Wait for infrastructure to be ready
k8s_resource('postgres', labels=['infra'])
k8s_resource('nats', labels=['infra'])
k8s_resource('redis', labels=['infra'])
k8s_resource('minio', labels=['infra'])

# --- Services ---

if dev_mode == 'fast':
    # Fast inner loop: go run directly, ~2s rebuild on code change
    # Services run on the host, connecting to infra in K8s via port-forwards

    # Port-forward infrastructure for local go run access
    k8s_resource('postgres', port_forwards=['5432:5432'])
    k8s_resource('nats', port_forwards=['4222:4222', '8222:8222'])
    k8s_resource('redis', port_forwards=['6379:6379'])
    k8s_resource('minio', port_forwards=['9000:9000', '9001:9001'])

    # Services will be added here as they are built in Phase 2+
    # Example:
    # local_resource('dsp', serve_cmd='go run ./cmd/dsp', deps=['cmd/dsp', 'pkg/'],
    #                labels=['services'])

else:
    # Container mode: Docker builds, deployed to K8s
    # Same as CI/staging/prod, slower but tests K8s-specific behaviour

    # Standard services (shared Dockerfile, CGO_ENABLED=0)
    services = ['dsp', 'ssp', 'adserver', 'tracker', 'pipeline', 'webhooks', 'ssai']

    for svc in services:
        docker_build(
            'adtech-' + svc,
            '.',
            dockerfile='build/Dockerfile',
            build_args={'SERVICE': svc},
            only=['cmd/' + svc, 'pkg/', 'go.mod', 'go.sum'],
        )

    # Exchange (multiple channel instances in prod, single --channel=all locally)
    docker_build(
        'adtech-exchange',
        '.',
        dockerfile='build/Dockerfile',
        build_args={'SERVICE': 'exchange'},
        only=['cmd/exchange', 'pkg/', 'go.mod', 'go.sum'],
    )

    # Gateway (embeds web assets)
    docker_build(
        'adtech-gateway',
        '.',
        dockerfile='build/Dockerfile.gateway',
        only=['cmd/gateway', 'pkg/', 'web/', 'go.mod', 'go.sum'],
    )

    # Reporting (CGO_ENABLED=1 for DuckDB)
    docker_build(
        'adtech-reporting',
        '.',
        dockerfile='build/Dockerfile.reporting',
        only=['cmd/reporting', 'pkg/', 'go.mod', 'go.sum'],
    )

    # Transcoder (requires FFmpeg)
    docker_build(
        'adtech-transcoder',
        '.',
        dockerfile='build/Dockerfile.transcoder',
        only=['cmd/transcoder', 'pkg/', 'go.mod', 'go.sum'],
    )

# --- Tilt Buttons (manual triggers) ---

# Seed data
local_resource('seed-minimal', cmd='go run ./cmd/seed --profile minimal',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'],
               auto_init=False)
local_resource('seed-standard', cmd='go run ./cmd/seed --profile standard',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'],
               auto_init=False)
local_resource('seed-stress', cmd='go run ./cmd/seed --profile stress',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'],
               auto_init=False)

# Simulation
local_resource('simulate-trickle', cmd='go run ./cmd/simulator --profile trickle',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['simulation'],
               auto_init=False)
local_resource('simulate-steady', cmd='go run ./cmd/simulator --profile steady',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['simulation'],
               auto_init=False)

# Database
local_resource('migrate', cmd='go run ./cmd/migrate',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'],
               auto_init=False)
local_resource('reset', cmd='go run ./cmd/migrate reset && go run ./cmd/migrate && go run ./cmd/seed --profile standard',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['data'],
               auto_init=False)

# Chaos testing
local_resource('chaos-kill-redis', cmd='kubectl -n adtech delete pod redis-0 --force',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['chaos'],
               auto_init=False)
local_resource('chaos-kill-nats', cmd='kubectl -n adtech delete pod nats-0 --force',
               trigger_mode=TRIGGER_MODE_MANUAL, labels=['chaos'],
               auto_init=False)
