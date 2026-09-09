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
    dayboundary, invoice-runner, account-closeout)
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
| `make stack-doctor` | **Run this FIRST when the stack looks dead** (esp. after laptop sleep). Staged diagnose+repair: RD app/VM wedge (rdctl restart) → k8s API → pod readiness → dead-tunnel signature (in-cluster OK but localhost resets = CNI-HOSTPORT jump rules stripped → bounce klipper svclb pods) → TB readyz. Idempotent, never touches data. Note: node on 192.168.5.x is BY DESIGN (RD pins --node-ip); never restart k3s in-VM as a "fix" — that's what strips the iptables jumps. |
| `make deploy SVC=pipeline` | Rebuild ONE image + rollout restart (SVC=dsp restarts all three DSP pods) |
| `make stack-down` | `helm uninstall` (add `PURGE=1` to also drop PVCs) |
| `make stack-images` | Just build the images |
| `make devconsole` | Host dev-loop UI (localhost:8099) — buttons over the targets above |

### Seed / reset / demo data

Getting data INTO a running stack (all Postgres unless noted). Migrations run
automatically inside `make stack-up` (helm hook); `make migrate` /
`make migrate-status` run them standalone.

| Command | What it does |
|---|---|
| `make seed` / `seed-minimal` / `seed-stress` | `go run ./cmd/seed --profile <p>` — **DB-direct** seed from `profiles/{dsps,publishers,deals,direct-sold}/*.yaml`. Needs the **owner** DB URL (cross-tenant inserts; the flipped `adtech_app` role can't do them under RLS). |
| **`make demo`** | `scripts/demo.sh`: wait for stack → seed standard → refresh warm caches → ~800 realistic auctions via the simulator. The "everyone runs this" rich setup. |
| **`make reset`** | `scripts/reset.sh`: full clean slate across ALL THREE stores — TRUNCATE Postgres tenant tables → TRUNCATE ClickHouse → Redis FLUSHDB → re-seed + re-populate. |
| `make traffic` / `simulate` / `simulate-trickle` / `simulate-burst` | `cmd/simulator` — generate live auction traffic (`DEMO_RPS` default 5). |

- **`cmd/seed` big-world knobs:** `--big-world-advertisers/-publishers/-campaigns-per/-placements-per` add an additive large world (each account funded + loginable). `BIGWORLD=1 go test -run TestBuildBigWorld` builds a ~50-campaign world via the **API** instead.
- **Runtime reseed (API):** `POST /dev/reset-and-reseed` on the gateway (debug-gated) = TRUNCATE tenant tables + Redis FLUSHDB + re-run `cmd/seed` + cache-invalidates. Backs the pub-simulator "Reset & reseed" button and the e2e harness. Runs the seed subprocess against the **admin/owner** URL (`DATABASE_ADMIN_URL`), because the app now connects as `adtech_app` (security #77) which can't cross-tenant seed.
- **API seeding (tests):** `harness.BuildBasicWorld`/`BuildAPIWorld` create accounts/campaigns/placements through real `POST /v1/api/*` calls (not DB-direct); `harness.SeedStandard` calls the reseed endpoint above.
- **External demo origins** (host processes; need stack up + seeded): `make demosite` (:9000 publisher), `make demoadv` (:9200 advertiser "Ford"), `cmd/extbidder` (external DSP).

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
