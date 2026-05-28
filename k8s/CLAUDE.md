# k8s/ - Kubernetes Manifests

All K8s manifests for the platform. Kustomize for environment overlays. Same manifests run locally (Colima + k3s) and in production.

## Structure

- `base/` - plain K8s manifests. Source of truth for what runs.
- `overlays/local/` - patches for local dev (single replicas, debug logging, filesystem storage, Mailpit)
- `overlays/staging/` - patches for staging (dev S3, reduced replicas)
- `overlays/prod/` - patches for production (replicas, resource limits, prod secrets, prod S3, separate Redis per service)

## What's in base/

### Services (long-running deployments)
`dsp/`, `ssp/`, `exchange/`, `adserver/`, `tracker/`, `reporting/`, `gateway/`, `pipeline/`, `webhooks/`, `billing/`

### Infrastructure
`postgres/`, `nats/`, `redis/`, `clickhouse/`

### Observability
`prometheus/`, `grafana/`, `jaeger/`, `loki/`, `promtail/`

### Other
`migrate/` (Job), `seed/` (Job), `simulator/` (Job), `mailpit/`, `ingress/` (Traefik routes)

### CronJobs
`cronjobs/rollup-*`, `cronjobs/optimise/`, `cronjobs/fraud-batch/`, `cronjobs/billing/`, `cronjobs/report-scheduler/`, `cronjobs/backup-*`

## Conventions

- Every service deployment has: `deployment.yaml`, `service.yaml`, `hpa.yaml`
- Network policies restrict service-to-service communication - see `docs/PLAN.md` -> "K8s Network Policies"
- Secrets: plaintext locally, SOPS-encrypted in staging/prod overlays
- New services need: deployment, service, HPA, network policy, and an entry in the Tiltfile
- HA runs everywhere including locally - see `docs/PLAN.md` -> "High Availability"

## Environment Differences

| Concern | Local | Staging | Prod |
|---|---|---|---|
| Object storage | Minio (S3-compatible) | S3 (dev bucket) | S3 (prod bucket) |
| Analytics | DuckDB (embedded) | DuckDB or ClickHouse | ClickHouse |
| Redis | 1 shared | 1 shared | Per-service |
| Email | Mailpit | Mailpit or SES sandbox | SES/Sendgrid |
| Secrets | Plaintext | SOPS-encrypted | SOPS-encrypted |
| Replicas | HPA min 1 | HPA min 2 | HPA min 2, higher max |
