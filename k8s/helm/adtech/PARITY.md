# Helm ↔ kustomize/Tilt parity

The chart's default values reproduce the local stack that
`kubectl kustomize k8s/overlays/local` + the Tiltfile's raw `k8s_yaml()` calls
deploy. Verified by:

```sh
scripts/helm-parity.sh
```

which renders both sides, normalizes them, and diffs per-resource. Current
result: **91 PASS, 0 DIFF, 0 MISSING, 0 EXTRA**.

Note the LEFT side of the diff is *kustomize overlay + the Tiltfile's raw
manifests*, not the overlay alone: the app services (dsp, ssp, exchange,
adserver, tracker, gateway, …) are **not** in the kustomize base — the
Tiltfile applies each `k8s/base/<svc>/*.yaml` file directly. Diffing against
the overlay alone would miss the entire serving path. The file list in
`scripts/helm-parity.sh` must stay in lockstep with the Tiltfile.

## Normalizations applied by the differ (cosmetic, not material)

- Resources keyed and ordered by `kind/namespace/name` — manifest ordering is
  ignored.
- Toolchain-managed metadata stripped from both sides: `helm.sh/*`,
  `app.kubernetes.io/{managed-by,instance,version}` labels; `meta.helm.sh/*`,
  `checksum/*`, `kubectl.kubernetes.io/last-applied-configuration`
  annotations; `metadata.creationTimestamp`; `status`.
- Defaults filled so semantically-equal specs compare equal: `replicas: 1`,
  Service `type: ClusterIP`, port `protocol: TCP`, `targetPort` = `port`.
- `env`, container `ports`, `volumeMounts`, `volumes`, Service `ports` sorted.
- Null values and empty maps dropped.

Everything else — images, env values, command/args, probes (every field),
resources, ConfigMap/Secret data byte-for-byte, schedules, selectors,
RBAC rules, PVC sizes — is compared strictly and currently matches.

## Intentional chart-side deltas (excluded from / invisible to the diff)

- **migrate hook Job** (`templates/migrate-job.yaml`): chart-only. There is no
  migrate Job manifest in `k8s/` today — Tilt runs `go run ./cmd/migrate` on
  the host. The chart needs migrations in-cluster, so it ships a
  `pre-install,pre-upgrade` hook using image `adtech-migrate` (build via
  `build/Dockerfile` with `SERVICE=migrate`). The parity script uses
  `helm template --no-hooks` to exclude it.
- **gateway-tls Secret is NOT in the chart.** Tilt creates it imperatively
  from mkcert-issued `dev/tls/*.pem` on every `tilt up`. The gateway volume
  references it with `optional: true` (identical to base), so its absence just
  disables the HTTPS listener. Create it out-of-band exactly as the Tiltfile
  does.
- **No reporting Ingress.** `k8s/base/reporting/ingress.yaml` exists but the
  Tiltfile never applies it, so the deployed local stack has none; the chart
  follows the Tiltfile. Restore with
  `services.reporting.ingress.hosts: [reporting.adtech.local]` if that file
  ever gets wired up.

## Verbatim file copies (keep in sync with k8s/base)

- `files/traefik-install.yaml` ← `k8s/base/traefik/install.yaml`
  (pre-rendered upstream traefik chart output, pinned in-repo; carries its own
  `traefik` namespace, unaffected by `.Values.namespace`).
- `files/grafana-dashboards-configmap.yaml` ← `k8s/base/grafana/dashboards.yaml`
  (dashboard JSON contains `{{service}}` legendFormat sequences that the
  template engine would eat, so it is `.Files.Get`'d; only `namespace: adtech`
  is rewritten for namespace overrides).

If the originals change, re-copy — the parity script will flag the drift.

## Secrets

Same mechanism as the kustomize base, i.e. **dev credentials in git**:
the `postgres-credentials` Secret (`adtech`/`adtech-local-dev`), Minio root
creds, ClickHouse password, and `S3_ACCESS_KEY`/`S3_SECRET_KEY` env values are
identical to `k8s/base`. Fine for local; staging/prod must override via
SOPS-managed values files or external secret machinery (see the header
comments in `values-staging.yaml` / `values-prod.yaml`).

## values-staging.yaml / values-prod.yaml provenance

`k8s/overlays/staging` and `k8s/overlays/prod` **do not exist in the repo**
(never created — checked full git history), despite `k8s/CLAUDE.md`
describing them. The staging/prod values files therefore translate the
*documented* deltas (the "Environment Differences" table in `k8s/CLAUDE.md`
and the "staging/prod overlays override…" comments inside the base
manifests): NATS back to the base 3-node cluster, serving-path replicas ≥ 2,
registry-mirror (and, for prod, Mailpit) off, TigerBeetle cache sized up.
Image tags, real S3/SMTP endpoints, public browser-facing URLs, and secrets
remain deploy-time overrides — each file's header lists them.

## Base-manifest inconsistencies noticed while porting (not fixed here)

- `k8s/base/reporting/ingress.yaml` is dead (see above).
- `k8s/CLAUDE.md` describes `billing/`, `migrate/`, `seed/`, `simulator/`
  dirs, per-service `hpa.yaml`, network policies, and `cronjobs/rollup-*`
  etc. — none of which exist under `k8s/`. The chart models what exists.
- `tracker` runs `replicas: 3` but pins `POD_NAME=tracker-0` on every
  replica, so all three pods share one per-pod config identity (the manifest
  comment says "safe for single-replica dev", which tracker no longer is).
  Carried over verbatim regardless.
