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
tenant-isolation hardening, and the riskiest. It needs an EXCLUSIVE stack for the
`DATABASE_URL` flip + full e2e, so it is not landed yet — but the design below is
**proven** against a real `NOBYPASSRLS` role in an isolated Postgres (2026-07-25;
all six isolation cases green: tenant read scoped, platform escape-hatch works,
unset-GUC returns empty with no error, cross-tenant read+write blocked).

### The crux: the platform loaders

Tenant queries set `app.current_account_id` in a tx (see
`pkg/store/postgres/postgres.go` `SetTenantContext` / `QueryRead`). But the
cross-tenant warm-cache loaders — `CampaignLoader`, `ContractLoader`,
`BalanceLoader`, `DealLoader`, `CreativeLoader`, `HouseAdLoader`, `FreqCapLoader`,
`OptOutLoader`, `AdsTxtLoader`, `PublisherLineItemLoader` (every `LoadAll` in
`pkg/store/postgres/`) — run with **no** GUC set and rely on the superuser
bypassing RLS. Under `NOBYPASSRLS` they'd filter to nothing. They need an
explicit, auditable escape hatch, NOT a blanket bypass role.

### Proven design: an `app.platform_read` escape-hatch GUC

1. ✅ **DONE — migration `065_rls_platform_read_hatch.sql`** rewrites every
   `tenant_isolation` policy (33 of them, 4 distinct forms incl. subquery/
   child-table scoping) to allow either the existing tenant match OR an explicit
   platform-read flag, preserving each policy's own logic. A `DO`-block loops
   `pg_policies` and, per policy: makes `current_setting('app.current_account_id')`
   missing_ok (`, true`) so a platform-read tx matches nothing instead of
   erroring, then OR-s in `current_setting('app.platform_read', true) = 'on'`.
   Idempotent; the Down reverses it via regexp. **No-op under the current
   superuser** (policy expressions aren't evaluated when RLS is bypassed), so it's
   safe to land ahead of the role flip — which it is.

   Proven (2026-07-25) against the REAL schema in a throwaway Postgres + a real
   `NOBYPASSRLS` role: on `data_providers`, all five cases green — tenant read
   scoped, platform_read=on sees all, unset-GUC empty (no error), cross-tenant
   read + write blocked. Down/Up cycle preserves the subquery-form policies.

2. **Create the limited role** (idempotent; migration runs as owner/superuser):
   ```sql
   DO $$ BEGIN
     IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='adtech_app') THEN
       CREATE ROLE adtech_app LOGIN NOSUPERUSER NOBYPASSRLS;
     END IF;
   END $$;
   -- password set out-of-band (SOPS secret), not in the migration
   GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO adtech_app;
   GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO adtech_app;
   ALTER DEFAULT PRIVILEGES IN SCHEMA public
     GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO adtech_app;
   ALTER DEFAULT PRIVILEGES IN SCHEMA public
     GRANT USAGE ON SEQUENCES TO adtech_app;
   ```
   (`FORCE ROW LEVEL SECURITY` is only needed if the querying role owns the table;
   `adtech_app` won't own anything, so plain `ENABLE` — already in place — suffices.)

3. **Add a `QueryPlatform` store helper + rewire the platform loaders.** ~16
   files call the raw `s.read.QueryContext(...)` (grep `\.read\.Query` in
   `pkg/store/postgres/`). These need a helper that runs the query inside a read
   tx with `SET LOCAL app.platform_read = 'on'`.

   ⚠️ **Two traps — this is the crux, do it with integration tests, not blind:**
   - **Classify every `.read.Query` call site first.** NOT all 16 are
     cross-tenant platform loads — some are tenant-scoped reads that filter by an
     `account_id` in the WHERE. Switching one of *those* to `platform_read` would
     BYPASS tenant isolation — the exact bug this task fixes. Only the `LoadAll`
     warm-cache loaders (CampaignLoader, BalanceLoader, DealLoader, …) get
     `QueryPlatform`; audit each of the others.
   - **`lib/pq` does not buffer rows past tx commit.** `QueryRead` today commits
     its tx *before* returning `*sql.Rows` (works only because tenant reads are
     small / already drained). `QueryPlatform` loads can be large (all campaigns),
     so it must NOT commit before the caller scans — pass a `func(*sql.Rows)`
     callback that runs inside the tx, or return a closer that commits after
     scan. Don't copy `QueryRead`'s commit-then-return shape.

   Under the superuser this is a behavioural no-op (RLS bypassed either way), so it
   can ship ahead of the flip. Keep the flag confined to `QueryPlatform` — grep
   for `platform_read` should return only that helper + this migration.

4. **Flip `DATABASE_URL`** to `adtech_app` (EXCLUSIVE stack) and run the full e2e
   suite: tenant-isolation (`rls_test.go`), the money loop, pacing, and a warm-
   cache refresh (proves the platform loaders still load cross-tenant). Migrations
   keep pointing at the owner/superuser `DATABASE_URL` (a separate env for the
   migrate job) so DDL still works.

## 4. Image supply chain (pinning + scanning)

**DONE (committed, no deploy needed — only affects freshly-built images / CI):**
- ✅ Base images pinned by digest in every `build/Dockerfile*`
  (`golang:1.25-alpine@sha256:…`, `alpine:3.20@sha256:…`). Re-resolve with
  `docker buildx imagetools inspect <img> --format '{{.Manifest.Digest}}'` when
  bumping the tag.
- ✅ Trivy CI job (`supply-chain-scan` in `.github/workflows/ci.yml`): Dockerfile
  misconfig scan + fixable HIGH/CRITICAL dependency CVEs, both fail the build.

**Still deferred (need a controlled deploy — changing an infra image string
restarts that StatefulSet, e.g. a postgres/nats bounce):**
1. Pin infra image tags in `values.yaml` (postgres/nats/redis/clickhouse) to
   digests, like Minio already is. **Do this during a planned maintenance
   window** — it rolls the stateful infra pods. Pin to the digest of the
   currently-running image to keep the bits identical.
2. Tag app images with the git SHA (not `:latest`) in CI; set
   `imagePullPolicy: Always` in prod.

## 5. Minor / accepted

- `promtail` init runs `privileged: true` to raise node inotify sysctls (needed
  for high-volume log shipping at scale). Accepted; the alternative is a
  node-level sysctl tune (kubelet `allowedUnsafeSysctls` / a DaemonSet) so the
  log shipper doesn't need privilege.
- Content-Security-Policy is not set on the gateway — the portal templates use
  inline `<script>`/`<style>`; a real CSP needs a nonce/hash pass over the
  templates first.
