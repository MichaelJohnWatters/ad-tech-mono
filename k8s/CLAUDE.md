# k8s/ - Kubernetes Manifests & Helm Chart

The platform deploys via the **Helm chart at `k8s/helm/adtech/`** onto
**Rancher Desktop (k3s)** locally. The same chart is the deploy artifact for
staging/prod (values files per env).

> Historical note: the stack used to run via Tilt + kustomize on OrbStack
> (before that, Colima). `k8s/base/` is retained as the manifest source the
> chart was derived from — `scripts/helm-parity.sh` proved field-level parity
> at cutover (91 resources). New changes go in the CHART; base is frozen.

## Structure

- `helm/adtech/` — the umbrella chart. **Source of truth for what runs.**
  - `values.yaml` = local dev defaults (single replicas, Mailpit, Minio,
    plaintext dev creds, `localExpose` klipper-lb localhost ports)
  - `values-staging.yaml` / `values-prod.yaml` — env overrides (real S3/SMTP,
    replicas, no localhost exposure; CI supplies image tags + SOPS secrets)
  - `templates/services.yaml` — ONE generic Deployment+Service template
    driven by the `services:` map (probes default `/healthz` + `/readyz`)
  - `templates/infra/`, `templates/observability/` — per-component templates
  - `templates/cronjobs.yaml` — from the `cronjobs:` map (batch-conductor,
    dayboundary)
  - `templates/migrate-job.yaml` — goose migrations as a post-install/upgrade
    hook (init container waits for postgres)
  - `templates/localhost-lb.yaml` — `<svc>-lb` LoadBalancer services; k3s
    klipper-lb binds them on localhost, replacing Tilt's port_forwards
  - `PARITY.md` — cutover parity notes + known base oddities
- `base/` — frozen pre-Helm manifests (parity reference only)
- `overlays/local/` — frozen kustomize overlay (parity reference only)

## Dev loop (Makefile)

| Command | What it does |
|---|---|
| `make stack-up` | Build all images (`scripts/stack-images.sh`) + `helm upgrade --install adtech` + gateway-tls secret |
| `make deploy SVC=pipeline` | Rebuild ONE image + rollout restart (SVC=dsp restarts all three DSP pods) |
| `make stack-down` | `helm uninstall` (add `PURGE=1` to also drop PVCs) |
| `make stack-images` | Just build the images |
| `make devconsole` | Host dev-loop UI (localhost:8099) — buttons over the targets above |

Requirements: Rancher Desktop running (moby engine, k8s enabled, built-in
traefik DISABLED — the chart ships its own), kubectl context
`rancher-desktop`. Image builds are host Go cross-compiles baked into tiny
alpine images (`build/Dockerfile.dev`); exceptions: gateway (bakes seed +
web + profiles), transcoder (ffmpeg), reporting (in-image Alpine CGO build for
tigerbeetle-go — the glibc go-duckdb build was retired in ADR 0006), migrate
(prod-style in-image build).

## Conventions

- Every app service gets an entry in the chart's `services:` map — env vars,
  ports, probes, resources all live in values, not new templates.
- Local ports: add a `localExpose.services` entry (host port = the port the
  e2e harness/portals expect; keep the historical Tilt port map).
- Secrets: plaintext dev creds in local values only; staging/prod come from
  deploy-time overrides (SOPS) — never commit real credentials.
- New NATS subjects / service dependencies → update `docs/PLAN.md` and the
  architecture diagram (see root CLAUDE.md diagram rule).

## Environment Differences

| Concern | Local | Staging | Prod |
|---|---|---|---|
| Object storage | Minio (S3-compatible) | S3 (dev bucket) | S3 (prod bucket) |
| Analytics | ClickHouse (hot) + Parquet export cold (s3()) | same | ClickHouse |
| Email | Mailpit | Mailpit or SES sandbox | SES/Sendgrid |
| Secrets | Plaintext values | SOPS at deploy | SOPS at deploy |
| Localhost LB services | on | off | off |
