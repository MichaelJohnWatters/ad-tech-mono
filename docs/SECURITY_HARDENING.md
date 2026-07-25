# Security hardening runbook (deferred infra items)

The security-audit pass closed the critical application holes (auth on the
control plane, dev endpoints off in prod, security headers, CSRF, JWT alg check,
RLS policies) — those are committed and guarded by e2e regression tests. See
`docs/DEPLOY.md` Step 8 for the secrets + enforcement checklist.

This file covers the **infrastructure** items that were intentionally *deferred*
because they need a full `make stack-up` + e2e run to validate (adding them blind
would crash the stack). Each is committed as OFF-by-default scaffolding; this is
the runbook to turn them on. **Do each in staging first.**

---

## 1. Non-root containers + `securityContext` (values: `global.hardenedContainers`)

The gated `securityContext` (runAsNonRoot / drop ALL caps / seccomp) is in
`templates/services.yaml`. It **cannot be enabled until the images run as a
non-root user**, or `runAsNonRoot: true` refuses to start the pods.

**Steps:**
1. Add a non-root user to every `build/Dockerfile*` final stage, e.g.:
   ```dockerfile
   RUN adduser -D -u 1000 app
   USER 1000:1000
   ```
   Gateway note: it reads `web/templates` + `web/static` (world-readable — fine).
   Check any service that writes to disk uses `/tmp` (writable by non-root).
2. `make stack-up` (default: hardening still off) → confirm all pods `Running` as
   non-root works at the image level.
3. Enable: `--set global.hardenedContainers=true` (or the prod values flag) and
   redeploy. Watch for `CreateContainerConfigError` / crash-loops (a service that
   needs root or a low port).
4. Run the full e2e suite green.
5. Follow-up: `readOnlyRootFilesystem: true` + per-service `emptyDir` scratch
   mounts (omitted for now — needs per-service validation).

## 2. NetworkPolicies (values: `global.networkPolicies`)

`templates/networkpolicies.yaml`: default-deny **ingress** + allow same-namespace
(the app mesh + prometheus scrape) + allow the Traefik namespace. Egress is left
open (restricting it is a separate step — it's where DNS/managed-store/S3 breakage
lives).

**Steps:**
1. `--set global.networkPolicies=true` in staging.
2. Verify: a browser request through Traefik still serves; an auction still runs
   (gateway→ssp→exchange→dsp→adserver→tracker all cross-pod); prometheus targets
   stay `up`; `/readyz` green everywhere.
3. Confirm cross-namespace ingress is now denied (e.g. a pod in `default` can't
   curl `gateway.adtech:8080`).
4. Follow-up: tighten to per-service policies + restrict egress to just the infra
   pods + DNS + your managed-store/S3 CIDRs.

## 3. Downgrade the app DB role from SUPERUSER (the real RLS fix)

Today the app connects as `adtech` with `rolsuper=t, rolbypassrls=t`, so **all
RLS is bypassed by the app** — the 20+ tenant_isolation policies (incl. the three
added in migration 064) are a no-op safety net. Fixing this is the highest-value
tenant-isolation hardening, and the riskiest.

**Steps (staging, carefully):**
1. Create a limited role: `CREATE ROLE adtech_app LOGIN PASSWORD '…' NOSUPERUSER
   NOBYPASSRLS;` and `GRANT` only the needed table privileges (SELECT/INSERT/
   UPDATE/DELETE on the app tables, USAGE on sequences). Migrations keep running
   as the owner/superuser.
2. Point the app `DATABASE_URL` at `adtech_app`.
3. **Cross-tenant warm-cache loaders break here** — `CampaignLoader`,
   `ContractLoader`, `BalanceLoader` load platform-wide with no tenant context, so
   RLS now filters them to nothing. Give the loaders a bypass: either a dedicated
   role with `BYPASSRLS` for those queries, or `ALTER TABLE … FORCE ROW LEVEL
   SECURITY` off + a policy that permits a "platform" GUC. Audit every store path
   that must set `app.current_account_id`.
4. Run the full e2e suite — tenant-isolation tests (`rls_test.go`) + the money
   loop + pacing must stay green.

## 4. Image supply chain (pinning + scanning)

Only Minio is pinned by digest; base + infra images float, and there's no scan.

**Steps:**
1. Pin base images by digest in `build/Dockerfile*`:
   `FROM golang:1.25-alpine@sha256:…` / `FROM alpine:3.20@sha256:…` (get the
   digest with `docker buildx imagetools inspect <img>` or `crane digest`).
2. Pin infra image tags in `values.yaml` (postgres/nats/redis/clickhouse) to
   digests, like Minio already is.
3. Tag app images with the git SHA (not `:latest`) in CI; set
   `imagePullPolicy: Always` in prod.
4. Add a Trivy (or Grype) scan step in CI that fails on HIGH/CRITICAL CVEs.

## 5. Minor / accepted

- `promtail` init runs `privileged: true` to raise node inotify sysctls (needed
  for high-volume log shipping at scale). Accepted; the alternative is a
  node-level sysctl tune (kubelet `allowedUnsafeSysctls` / a DaemonSet) so the
  log shipper doesn't need privilege.
- Content-Security-Policy is not set on the gateway — the portal templates use
  inline `<script>`/`<style>`; a real CSP needs a nonce/hash pass over the
  templates first.
