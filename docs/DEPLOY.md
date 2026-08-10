# Deploying to the cloud (go-live runbook)

The same Helm chart (`k8s/helm/adtech`) that runs locally deploys to a cloud
k8s cluster. Going live is a **values swap + a handful of cloud-account
actions** — the chart is already parameterized for it (TLS, domain, managed
persistence, rate limiting, public URLs). This runbook lists both.

> **Legend:** 🟢 = turnkey (chart/values, done for you) · 🔵 = your cloud account
> (I can't provision these from the repo).

## Staging quickstart (host-agnostic) 🟢/🔵

Staging validates the chart on a real node without the managed-services cost:
**all data services in-cluster** (Postgres/Redis/NATS/ClickHouse as pods), only
object storage on real S3, no registered domain required (sslip.io). The config
is ready — `values-staging.yaml` + the SOPS overlay do it; you supply a node.

1. **Images** — run the **build-push** workflow (Actions → build-push → Run) to
   push `ghcr.io/<owner>/adtech-<svc>` images. Set `global.image.registry`/`tag`
   in `values-staging.yaml`.
2. **Node** 🔵 — any fresh k3s node (`curl -sfL https://get.k3s.io | sh -`).
   Copy its kubeconfig locally; `kubectl config use-context` it.
3. **cert-manager** 🔵 — `kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml`.
4. **Domain** — no DNS purchase: set `global.domain: <node-ip-dashed>.sslip.io`
   (e.g. `203-0-113-5.sslip.io`) and match the seller-domain / public-URL
   `REPLACE.sslip.io` placeholders in `values-staging.yaml`. Let's Encrypt HTTP-01
   validates sslip.io. (Have a real domain? Use it instead.)
5. **Secrets** — set up SOPS (Step 4 below), fill + encrypt the staging overlay.
6. **Deploy** — `make deploy-staging` (helm upgrade with values-staging + the
   SOPS-decrypted secrets; targets the current kubectl context). Seed with
   `make seed` against the node's owner DB URL.

The rest of this runbook is the prod path (managed stores, real domain); staging
reuses the same steps with the in-cluster + sslip.io shortcuts above.

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
- Images built + pushed to a registry your cluster can pull. The **build-push**
  GitHub Actions workflow pushes every service to `ghcr.io/<owner>/adtech-<svc>`;
  point the chart at them with ONE knob — `global.image.registry` +
  `global.image.tag` (see values-staging.yaml). No per-service overrides needed.

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

## Step 4 — secrets (SOPS + age) 🔵

Real credentials never enter git in plaintext. They live in a SOPS-encrypted
**Helm values overlay** that's layered last at deploy time — so `sops -d` feeds
the decrypted values straight into `helm upgrade` without ever writing plaintext
to disk. Scaffolding: `.sops.yaml` (creation rules) +
`k8s/helm/adtech/secrets/staging.secrets.example.yaml` (the template) + the
`secrets-*` / `deploy-staging` Make targets.

**One-time key setup** (`brew install sops age` first):

```bash
age-keygen -o age.key          # prints your public recipient (age1...)
# paste the age1... recipient into .sops.yaml (replace the placeholder)
# keep age.key OUT of git (.gitignored) — store it in a password manager / CI secret
```

**Fill + encrypt the overlay:**

```bash
cp k8s/helm/adtech/secrets/staging.secrets.example.yaml \
   k8s/helm/adtech/secrets/staging.secrets.yaml
$EDITOR k8s/helm/adtech/secrets/staging.secrets.yaml   # fill the REPLACE-* values
make secrets-encrypt ENV=staging                        # → staging.secrets.enc.yaml (commit THIS)
```

The overlay carries the chart-consumed secrets: real-S3 `accessKey`/`secretKey`
and the in-cluster Postgres/ClickHouse passwords. **App-level signing keys**
(`jwt_signing`, `hmac_tracker`, `hmac_conversion`, `pgp_private`,
`adcert_ed25519`) live in the Postgres `secrets` table — `make seed` mints
dev-deterministic ones; for real keys insert them after first boot or add
`SECRETS_ENCRYPTION_KEY` / `GATEWAY_JWT_SIGNING_KEY` as service `extraEnv` in the
overlay. Seed at least one `jwt_signing` or the gateway refuses to boot (that's
the point of `GATEWAY_REQUIRE_AUTH=true`). Rotate the `adtech_app` role password
per the security note below.

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

### RLS app role (`adtech_app`) — rotate the password 🔴

Security #77: app services connect as the least-privilege **`adtech_app`**
(NOSUPERUSER, NOBYPASSRLS) role so RLS tenant isolation actually enforces; only
the **migrate job** (and the gateway's `DATABASE_ADMIN_URL`, for dev reset)
connect as the owner. Migration 067 creates the role; migration 069 sets a
**local dev** password *only when none is set*. In staging/prod you MUST:

1. Set the real password on the role right after migrate (it won't be clobbered
   on future migrate runs — 069 is guarded on `rolpassword IS NULL`):
   ```bash
   kubectl -n adtech exec postgres-0 -- psql -U <owner> -d adtech \
     -c "ALTER ROLE adtech_app PASSWORD '<value from SOPS>';"
   ```
   (Managed Postgres/RDS: run the `ALTER ROLE` via your admin connection.)
2. Point every app service's `DATABASE_URL` in `values-{staging,prod}.yaml` at
   `adtech_app` with that same secret; keep the migrate job on the owner URL.
3. Confirm enforcement: `E2E_APP_POSTGRES_URL=<adtech_app url> go test
   ./tests/e2e -tags=e2e -run TestRLSIsolation` — all 6 subtests must pass.

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

## Step 8 — security hardening (do NOT skip) 🔵

The local stack ships permissive by design. `values-prod.yaml` closes the biggest
gaps automatically (`GATEWAY_REQUIRE_AUTH=true`, `global.debugEndpoints: false` →
`/debug/*` + the dev token minter `/v1/auth/token` + the `/dev/*` console are OFF).
The rest are environment-specific and MUST be supplied out-of-band via SOPS/env:

**Secrets & keys (SOPS at deploy):**
- `SECRETS_ENCRYPTION_KEY` (32 bytes, `openssl rand -hex 32`) — WITHOUT it, the
  secrets table is stored **plaintext** in Postgres. Set it on every service that
  reads secrets (gateway/ssp/dsp/exchange/pipeline).
- `PLATFORM_ROOT_PASSWORD` — gates the one-shot `/v1/auth/bootstrap` that mints the
  first operator key. Unset ⇒ bootstrap 503s.
- `jwt_signing` secret (or `GATEWAY_JWT_SIGNING_KEY`) — real 32B random; without it
  auth is bypassed (and with `require_auth=true` the gateway refuses to boot).
- Real `DATABASE_URL`, ClickHouse password, S3 keys, `SSP_HOUSEHOLD_SALT` (unique &
  STABLE — it salts CTV household hashing; the dev default collides across envs).

**Rotate the dev-seeded keys** if you ever ran `cmd/seed` against the environment:
the HMAC pixel-signing key, the ads.cert Ed25519 key, and delete the
`dev-api-key-do-not-use-in-prod` operator key. Use the secrets console / rotation
(overlapping-rotation is supported, so no downtime).

**Turn on anti-spoofing enforcement (staged — strict needs the identity bundle):**
Enforcement defaults to off/warn because *strict without the supporting setup
no-bids real traffic*. Enable in order, watching logs at `warn` before `strict`:
1. **schain** — set `SSP_SELLER_DOMAIN` (+ `SSP_SELLER_ID`) so the SSP *originates*
   a SupplyChain, then `exchange.schain_enforcement=strict`. (Strict with no
   seller_domain = every auction no-bids.)
2. **ads.cert** — DSPs already fetch the exchange keyset (`DSP_ADCERT_KEY_URL`, baked
   in); rotate in a real `adcert_ed25519` key, then `dsp.adcert_enforcement=warn→strict`.
3. **ads.txt** — set `exchange.adstxt_seller_domain/_id`, get publishers to add the
   line (surfaced in the publisher portal + `/v1/api/integration/adstxt`), then
   `exchange.adstxt_enforcement=warn→strict`.
4. **tracker HMAC** — with a real signing key deployed, `tracker.signature_validation=true`.

These are live-tier config: set once via the Config UI (Staff → Config) or the
config API; on a fresh cluster you can also pre-seed them.

**Client-IP topology (household ids, fraud checks, rate limits):**
Every consumer of "who is this viewer" reads the shared trusted-proxy parser
(`pkg/clientip`): the client IP is taken N entries from the RIGHT of
X-Forwarded-For, where N = the number of trusted proxies in front of the app.
Set all of these to match your real chain — **0** = ingress only (default),
**1** = Cloudflare in front of the ingress:
- `ssp.trusted_proxy_hops` (household-id derivation + identity fingerprint)
- `tracker.trusted_proxy_hops` (fraud-check IP — blocklists, datacenter CIDRs)
- `<svc>.ratelimit_trusted_proxy_hops` (per-IP rate limits)

Wrong in either direction hurts: too low reads a proxy address (a whole site
collapses into one "household" → fill craters on household-capped campaigns);
too high trusts a client-forgeable XFF entry (bots dodge IP blocklists, one
device mints fresh households at will).

Also review `ssp.ip_override_allowlist`: the `?ip=` end-user-IP override (used
by server-side callers that legitimately know the device IP — SSAI, server-side
publisher tags) is honoured only from callers inside this CIDR list. The
default is private ranges (right for in-cluster SSAI); add publisher server
egress ranges that send server-to-server ad requests, and nothing else — a
public browser's `?ip=` must stay ignored or household frequency caps stop
being a real control.

## Go-live checklist

- [ ] 🔵 Domain registered + DNS at the ingress LB (or Cloudflare)
- [ ] 🔵 cert-manager installed; 🟢 `createIssuers: true` + email set
- [ ] 🔵 Managed Postgres/Redis/S3 provisioned; 🟢 `enabled: false` + endpoints (SOPS)
- [ ] 🟢 `global.domain` + public URLs set to your domain
- [ ] 🔵 `jwt_signing` secret seeded (gateway won't boot without it)
- [ ] 🟢 `helm upgrade --install … -f values-prod.yaml`
- [ ] 🔵 Cloudflare in front (optional but recommended)
- [ ] 🔵 `*.trusted_proxy_hops` set to the real proxy-chain depth (1 with Cloudflare) + `ssp.ip_override_allowlist` pruned to server-side caller ranges
- [ ] Verify: `https://gateway.<domain>/healthz`, a test auction, tracked impression
