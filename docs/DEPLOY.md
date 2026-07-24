# Deploying to the cloud (go-live runbook)

The same Helm chart (`k8s/helm/adtech`) that runs locally deploys to a cloud
k8s cluster. Going live is a **values swap + a handful of cloud-account
actions** — the chart is already parameterized for it (TLS, domain, managed
persistence, rate limiting, public URLs). This runbook lists both.

> **Legend:** 🟢 = turnkey (chart/values, done for you) · 🔵 = your cloud account
> (I can't provision these from the repo).

## What the chart already does for you 🟢

| Concern | How | Where |
|---|---|---|
| One-knob domain | every ingress host + cert SAN derives from `global.domain` | `values.yaml` / `values-prod.yaml` |
| HTTPS | Traefik `websecure`/443 + cert-manager Let's Encrypt | `templates/services.yaml`, `templates/cert-issuers.yaml` |
| Auth enforced | `GATEWAY_REQUIRE_AUTH=true` (no dev bypass) | `values-prod.yaml` |
| Rate limiting | per-IP token bucket on adserver/ssp/gateway | `<svc>.ratelimit_rps` |
| Managed persistence | `infra.<x>.enabled=false` → use your managed store | `values-prod.yaml` |
| Durable volumes | `storageClass: gp3` on what stays in-cluster | `values-prod.yaml` |
| HTTPS ad pixels | public tracker/media URLs (no mixed content) | `values-prod.yaml` extraEnv |

## Prerequisites 🔵

- A cloud k8s cluster (EKS/GKE/AKS/k3s) + `kubectl` + `helm`.
- A **registered domain** you control DNS for.
- **cert-manager** installed in the cluster (`kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml`).
- Managed data stores provisioned (see step 2).
- Images built + pushed to a registry your cluster can pull (override `services.<svc>.image`).

## Step 1 — set your domain 🟢/🔵

In `values-prod.yaml`:

```yaml
global:
  domain: your-domain.com        # every host becomes gateway.your-domain.com, etc.
```

Then point DNS 🔵 at the ingress LoadBalancer's external IP (get it from
`kubectl -n traefik get svc traefik`). Easiest: a wildcard `*.your-domain.com`
+ apex `your-domain.com` A/AAAA record (or Cloudflare — see step 6).

## Step 2 — provision + wire managed persistence 🔵/🟢

Recommended: managed Postgres (RDS/CloudSQL) + real S3; keep TigerBeetle + NATS
in-cluster (clustered, durable class); ClickHouse managed or in-cluster.

In `values-prod.yaml`, disable the in-cluster store and point services at the
managed endpoint (via SOPS-encrypted secrets, not plaintext):

```yaml
infra:
  postgres: { enabled: false }   # use RDS; set DATABASE_URL on every service + the migrate job
  redis:    { enabled: false }   # use ElastiCache; set REDIS_URL
  minio:    { enabled: false }   # use real S3; set S3_ENDPOINT + creds
  # kept in-cluster, on durable storage:
  tigerbeetle: { replicas: 3, storageClass: gp3 }
  clickhouse:  { storageClass: gp3 }
  nats:        { storageClass: gp3 }
```

If you self-host Postgres instead, enable the backup CronJob
(`backup.postgres.enabled: true`) or rely on your managed DB's snapshots/PITR.

## Step 3 — TLS certificates 🟢

cert-manager issues real Let's Encrypt certs automatically. In `values-prod.yaml`:

```yaml
tls:
  enabled: true
  certManager:
    clusterIssuer: letsencrypt-prod
    createIssuers: true            # render the ClusterIssuers for you
    email: ops@your-domain.com     # LE account email
```

Validate with the **staging** issuer first (`clusterIssuer: letsencrypt-staging`)
to avoid Let's Encrypt rate limits, then switch to prod.

## Step 4 — secrets 🔵

Provide via SOPS at deploy time (never commit): `jwt_signing` secret (or
`GATEWAY_JWT_SIGNING_KEY`), managed-store credentials, SMTP creds. Seed at least
one `jwt_signing` secret or the gateway refuses to boot (that's the point of
`GATEWAY_REQUIRE_AUTH=true`).

## Step 5 — public browser-facing URLs 🟢/🔵

`values-prod.yaml` already sets these to `https://…` so served-ad pixels aren't
blocked as mixed content on an HTTPS publisher page. **Match them to your domain**
(they're plain strings — env can't interpolate `global.domain`):

```yaml
services:
  adserver:            { extraEnv: [{ name: ADSERVER_TRACKER_URL, value: "https://tracker.your-domain.com" }] }
  publisher-adserver:  { extraEnv: [ ... https://tracker.your-domain.com, https://pubad.your-domain.com ] }
  gateway:             { extraEnv: [ ... GATEWAY_PUBLIC_TRACKER_URL, GATEWAY_CREATIVES_STORE_URL=https://cdn.your-domain.com/... ] }
```

## Step 6 — deploy 🟢

```bash
helm upgrade --install adtech k8s/helm/adtech -f k8s/helm/adtech/values-prod.yaml
```

Migrations run as a pre-install/upgrade hook against `DATABASE_URL`. Verify:

```bash
kubectl -n adtech get pods
curl https://gateway.your-domain.com/healthz
```

## Step 7 — Cloudflare edge (recommended) 🔵

Put Cloudflare in front for free TLS termination at the edge, DDoS/bot
protection, and caching. Proxy the ingress hosts through Cloudflare (orange
cloud); it becomes the WAF the per-pod rate limiter can't be. If you terminate
TLS at Cloudflare, you can run cluster-internal TLS in "flexible/full" mode.

## The external origins (publisher + DSP) 🟢/🔵

These deploy as **separate** workloads (a real publisher / DSP is never in your
ad-tech cluster):

- **`cmd/demosite`** (external publisher) — deploy anywhere with
  `cmd/demosite/deploy/demosite.yaml`; set `DEMOSITE_*` to your public URLs.
- **`cmd/extbidder`** (external DSP) — run it, then add its public URL to the
  `exchange.dsp_endpoints` live config.

## Go-live checklist

- [ ] 🔵 Domain registered + DNS at the ingress LB (or Cloudflare)
- [ ] 🔵 cert-manager installed; 🟢 `createIssuers: true` + email set
- [ ] 🔵 Managed Postgres/Redis/S3 provisioned; 🟢 `enabled: false` + endpoints (SOPS)
- [ ] 🟢 `global.domain` + public URLs set to your domain
- [ ] 🔵 `jwt_signing` secret seeded (gateway won't boot without it)
- [ ] 🟢 `helm upgrade --install … -f values-prod.yaml`
- [ ] 🔵 Cloudflare in front (optional but recommended)
- [ ] Verify: `https://gateway.<domain>/healthz`, a test auction, tracked impression
