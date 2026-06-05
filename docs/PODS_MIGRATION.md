# Pods Migration — DEV_MODE=container

> **Status (2026-06-04):** LIVE. All 9 service deployments + reporting
> (local) run on Colima k3s under `tilt up`. Blocker 1 (test inversion)
> remains — e2e suite still fails on the same 19 cases — but the dev
> stack itself is podified. See "Why it was parked" below for what's
> still outstanding.
>
> **Critical runtime requirement: Colima vmType MUST be `qemu`, not `vz`.**
> The Apple Virtualization.Framework backend (`vz`) crashed the cluster 9
> times during the migration under sustained docker-build load on macOS
> 14. QEMU is slower to boot but stable. The vmType is locked at VM
> creation — you cannot switch in place. To switch:
>
> ```
> colima delete --force
> colima start --vm-type qemu --cpu 6 --memory 24 --disk 100 --kubernetes
> ```
>
> Migration 024 (`pod_owned_schema.sql`) was patched to self-bootstrap
> the `service_registry` table — previously it ALTERed a table that's
> normally created lazily by `pkg/config/registry.go` at first pod
> boot, which failed on a fresh cluster where the migrate job runs
> before any service starts.

## What's in the repo right now

The migration artifacts stayed in the repo even after the Tiltfile was
reverted to `local_resource` for now. Re-enabling the migration is just
re-applying the Tiltfile changes from git history of this branch.

- `pkg/config/setup.go` — env→config bridge for 10 service/infra URL keys
- `build/Dockerfile.dev` — copies pre-built host binary; pairs with Tilt live_update
- `scripts/setup-hosts.sh` — adds /etc/hosts entries for *.adtech.local
- `k8s/base/traefik/install.yaml` — Helm-rendered Traefik snapshot (still installed in the cluster)
- `k8s/base/{tracker,adserver,reporting,dsp,ssp,exchange,publisher-adserver,gateway}/` — Deployment + Service + Ingress for each service
- `tests/e2e/harness/hostproxy.go` — `HostReachableServer(handler)` helper for the host-bridge approach
- Tiltfile reverted to `local_resource` baseline + Traefik install retained

## Why it was parked

Two unresolved blockers:

### Blocker 1 — e2e tests use a pod→host inversion pattern

Most e2e tests spin up `httptest.NewServer` on the host (random
`127.0.0.1:NNNN` ports), then write those URLs into a podified
service's config (`exchange.dsp_endpoints`, pubad outbound URLs,
etc.). The service then has to call back to the host. **127.0.0.1
from inside a pod = the pod's own loopback**, not the host. 19
e2e tests fail with this pattern.

Two paths to fix:

1. **Production-shape (recommended):** convert fake DSPs / fake
   Prebid Servers in `tests/e2e/harness/fakedsp.go` into actual
   K8s pods. Tests register handlers via a controller pod's API.
   Pubad/exchange call those fake pods via in-cluster Service DNS.
   ~1 day refactor. After this, tests work the same on local dev,
   CI, and any real cluster.

2. **Host-bridge for dev (started but incomplete):** the
   `HostReachableServer` helper (`tests/e2e/harness/hostproxy.go`)
   binds httptest listeners to `0.0.0.0` and returns URLs with
   the host's IP (env-driven via `ADTECH_HOST_IP=192.168.5.2` on
   Colima). Pod deployments need `hostAliases` mapping
   `host.docker.internal` → ADTECH_HOST_IP if the test code
   references that hostname. Faster fix (~2h) but local-only;
   doesn't help CI.

### Blocker 2 — reporting CGO cross-compile

Reporting uses DuckDB which requires CGO. Cross-compiling CGO from
macOS to Linux/musl needs either:

- A docker-based build step (`docker run golang:1.25-alpine`) that
  produces the Linux+musl binary on the host's docker daemon, then
  Dockerfile.dev wraps it. **This works when Colima is stable but
  failed mid-build several times during the initial migration
  attempt (Colima docker daemon dropped).**
- A full Dockerfile.reporting build inside the cluster (current
  prod pattern). The Colima VM's network couldn't reach
  `proxy.golang.org` reliably for `go mod download` — error needs
  investigation.

Reporting is the only service that can't use the cleaner
Dockerfile.dev pattern.

## What was validated before parking

- ✅ Foundation: env-URL plumbing, Dockerfile.dev, /etc/hosts script, Traefik install
- ✅ Tracker as a pod (e2e green at 128s with only tracker podified)
- ✅ Pods can reach the host via Colima's host-from-VM IP `192.168.5.2`
- ✅ Traefik routes Ingress correctly (cluster-internal `200 OK`)
- 🟡 Adserver, DSP×3, SSP, Exchange, Publisher-Adserver, Gateway pod manifests apply cleanly but full e2e green requires the test-architecture fix
- 🔴 Reporting: blocked on CGO cross-compile reliability
- 🔴 19 e2e tests fail with pods running (host-network inversion)

## Resumption checklist

When you pick this back up:

1. Verify cluster state: `kubectl --context colima -n traefik get pods` (Traefik should be Running).
2. Decide on Blocker 1 strategy (production-shape fakes vs host-bridge).
3. Decide on Blocker 2 strategy for reporting.
4. Re-apply the Tiltfile changes from git history of the branch that includes this doc.
5. Migrate services one at a time, validating e2e after each.

## End-state goals

## End-state goals

1. Every service runs as a k8s Deployment + Service + Ingress in the `adtech` namespace
2. Traefik (already part of k3s) routes `gateway.adtech.local`, `dsp.adtech.local`, etc. by `Host` header
3. No port numbers in URLs — `http://gateway.adtech.local` serves the dashboard
4. Tilt `live_update` keeps the iteration loop fast — edit Go → ~5s to see the new behaviour in the running pod (rsync the compiled binary into the pod, not a full image rebuild)
5. `/etc/hosts` entries map all hostnames to `127.0.0.1`
6. Existing Dockerfiles (`build/Dockerfile`, `build/Dockerfile.gateway`, `build/Dockerfile.reporting`) reused as-is for prod-shape builds

## Key tactical decisions

- **Keep `port_forwards` on every podified service** — preserves the e2e harness, simulator UI, config manager UI with **zero code changes**. `localhost:8082` keeps working AND `dsp.adtech.local` works via Ingress.
- **Live_update via `sync + restart_container`** — Go binary rebuilt on host (~1-2s), rsync'd into pod, container restarts in <1s. No image rebuild needed.
- **Service-to-service URLs become env vars** (`DSP_URL`, `EXCHANGE_URL`, etc.) per Deployment. `pkg/config/setup.go:137-146` already absorbs env vars into the live config map; extend the same pattern. **No service-code changes** required.
- **DEV_MODE gates the migration** — `DEV_MODE=fast` (current default) keeps everything as host processes. `DEV_MODE=hybrid` with a `PODIFY=dsp,tracker` env list lets us pod some services and host-process others during the migration window.

## Migration order (lowest blast radius first)

1. **tracker** — fewest cross-service deps (NATS + Redis only); validates Dockerfile.dev + live_update loop
2. **adserver** — validates Minio dependency from inside cluster
3. **reporting** — validates CGO Dockerfile.reporting variant
4. **dsp + dsp-comp1 + dsp-comp2** — validates per-pod `DSP_PROFILE` + multi-Deployment-from-same-image pattern
5. **ssp** — validates service-to-service DNS resolution (calls exchange)
6. **exchange** — validates fan-out to multiple DSP pods via new `EXCHANGE_DSP_ENDPOINTS` env
7. **publisher-adserver** — depends on both ssp + adserver
8. **gateway** — last, since its proxy URLs touch every other service

After each step: `tilt up`, hit `/readyz` via port-forward, run the smallest relevant e2e subtest, commit. Full `test-e2e` at the end.

## Per-service manifest template (dsp example)

```yaml
# k8s/base/dsp/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: dsp, namespace: adtech, labels: { app: dsp } }
spec:
  replicas: 1
  selector: { matchLabels: { app: dsp } }
  template:
    metadata: { labels: { app: dsp } }
    spec:
      containers:
        - name: dsp
          image: adtech-dsp
          imagePullPolicy: IfNotPresent
          ports: [{ containerPort: 8082 }]
          env:
            - { name: POD_NAME, valueFrom: { fieldRef: { fieldPath: metadata.name } } }
            - { name: DATABASE_URL, value: "postgres://adtech:adtech-local-dev@postgres:5432/adtech?sslmode=disable" }
            - { name: NATS_URL, value: "nats://nats:4222" }
            - { name: REDIS_URL, value: "redis:6379" }
            - { name: LOKI_URL, value: "http://loki:3100" }
            - { name: DSP_PROFILE, value: "internal" }
          resources:
            requests: { cpu: 50m, memory: 64Mi }
            limits:   { cpu: 500m, memory: 256Mi }
          livenessProbe:  { httpGet: { path: /healthz, port: 8082 }, initialDelaySeconds: 5 }
          readinessProbe: { httpGet: { path: /readyz,  port: 8082 }, periodSeconds: 2 }
---
# k8s/base/dsp/service.yaml
apiVersion: v1
kind: Service
metadata: { name: dsp, namespace: adtech }
spec:
  selector: { app: dsp }
  ports: [{ port: 8082, targetPort: 8082, name: http }]
---
# k8s/base/dsp/ingress.yaml — Traefik (k3s default)
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: dsp
  namespace: adtech
  annotations: { traefik.ingress.kubernetes.io/router.entrypoints: "web" }
spec:
  rules:
    - host: dsp.adtech.local
      http:
        paths:
          - { path: /, pathType: Prefix, backend: { service: { name: dsp, port: { number: 8082 } } } }
```

## Per-service Tiltfile pattern

```python
docker_build('adtech-dsp', '.',
    dockerfile='build/Dockerfile.dev',
    only=['bin/dsp'],
    live_update=[
        sync('bin/dsp', '/app'),
        restart_container(),
    ])
local_resource('dsp-build',
    cmd='go build -o ./bin/dsp ./cmd/dsp',
    deps=['cmd/dsp', 'pkg/'], labels=['build'])
k8s_yaml(['k8s/base/dsp/deployment.yaml',
          'k8s/base/dsp/service.yaml',
          'k8s/base/dsp/ingress.yaml'])
k8s_resource('dsp',
    resource_deps=['dsp-build', 'postgres', 'redis'],
    port_forwards=['8082:8082'],
    readiness_probe=ready(8082),
    labels=['services'])
```

Iteration loop: edit `cmd/dsp/*.go` → `dsp-build` triggers (~1-2s) → Tilt sees `bin/dsp` change → live_update rsyncs into running pod → `restart_container()` (sub-second restart).

## Service-to-service URL plumbing

`pkg/config/setup.go:137-146` already absorbs `DATABASE_URL` env var and writes it back into live config as `database.url`. Extend the same pattern for service URLs:

```go
// In setup.go, after existing DATABASE_URL handling:
for _, mapping := range []struct{ env, key string }{
    {"DSP_URL", "dsp.url"},
    {"EXCHANGE_URL", "exchange.url"},
    {"SSP_URL", "ssp.url"},
    {"TRACKER_URL", "tracker.url"},
    {"REPORTING_URL", "reporting.url"},
    {"ADSERVER_URL", "adserver.url"},
    {"PUBLISHER_ADSERVER_URL", "publisher_adserver.url"},
    {"NATS_URL", "nats.url"},
    {"REDIS_URL", "redis.url"},
    {"EXCHANGE_DSP_ENDPOINTS", "exchange.dsp_endpoints"},
} {
    if v := os.Getenv(mapping.env); v != "" {
        cfg.SetLive(mapping.key, v)
    }
}
```

Service code keeps reading via `cfg.Get("dsp.url", routes.DefaultDSPURL)` — no changes.

## e2e test harness

`tests/e2e/harness/harness.go:50-67` builds URLs from `routes.Default*URL` (which all resolve to `localhost:<port>`). Migration preserves these via Tilt `port_forwards`. **Zero harness changes required.**

## Rollback

Tiltfile already gates on `dev_mode = os.getenv('DEV_MODE', 'fast')`. Reuse this:

- Migrate services one at a time inside the existing `if dev_mode == 'fast':` block.
- For each migrated service, keep the `local_resource(...)` block commented just above the new `docker_build` / `k8s_yaml` block with `# rollback: uncomment + delete the docker_build block below`.
- Per-service git commits → `git revert` rolls back one service without touching others.

## Effort estimate

| Phase | Hours |
|---|---|
| Foundation: `build/Dockerfile.dev`, `setup.go` env-var URL plumbing, ingress kustomization wiring, /etc/hosts entries, Traefik verification | 3 |
| First service end-to-end (tracker): manifests + Tiltfile block + live_update verified + e2e green | 3 |
| Per additional service (adserver, reporting, ssp, exchange, publisher-adserver, gateway): manifests + Tiltfile, ~1.5h each | 9 |
| DSP cluster (3 pods, profile env, per-pod ingress hostnames) | 2 |
| e2e harness validation + fix any port-forward/DNS surprises | 2 |
| Documentation: update `k8s/CLAUDE.md`, Tilt endpoints reference | 1 |
| Buffer for Colima image-pull/Traefik routing/cgroup quirks | 2 |
| **Total** | **~22 hours** |

## Critical files

- `Tiltfile:51-154` — current local_resource definitions
- `build/Dockerfile`, `build/Dockerfile.gateway`, `build/Dockerfile.reporting` — prod images
- `build/Dockerfile.dev` (new) — binary-only image for live_update
- `k8s/base/{tracker,adserver,reporting,dsp,ssp,exchange,gateway,publisher-adserver}/` — currently empty dirs, fill with deployment.yaml + service.yaml + ingress.yaml
- `k8s/base/kustomization.yaml:6-23` — add new service paths
- `pkg/config/setup.go:137-146` — env→config bridge to extend
- `pkg/routes/routes.go:228-284` — Default*URL constants stay unchanged (host-process fallback)
- `tests/e2e/harness/harness.go:50-67` — URL construction; keep unchanged
- `scripts/setup-hosts.sh` (new) — /etc/hosts entries for *.adtech.local
