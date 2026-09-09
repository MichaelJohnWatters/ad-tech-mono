# pkg/middleware - Shared HTTP Middleware

Every service's HTTP edge is assembled from here: auth (JWT + API key), tenant
scoping, reverse proxying, rate limiting, CORS/CSRF/security headers, Prometheus
metrics, pprof. No service hand-rolls any of these.

## Key Entry Points

- `Auth(signingKey, log, opts...)` (`auth.go`) — validates HS256 JWTs from
  `Authorization: Bearer` OR the `adtech_session` cookie (browser UI), injects
  `*auth.Claims` into context (`ClaimsFromContext` / `WithClaims`).
  `RequirePermission` / `RequirePermissionByMethod` gate RBAC (methods absent
  from the map → 405, so a new verb can't slip through ungated). `ParseSession`
  is the browser-page variant (redirect-to-login instead of JSON 401).
- `AuthAPIKey(cache, log)` (`api_key.go`) — validates `X-API-Key` against the
  secrets warm cache (purpose=api_key only); read the authorising key via
  `SecretFromContext`. GET-only `?api_key=` fallback for clickable dev links.
- `CallerScope(r)` (`scope.go`) — resolves the tenant `Scope` (account-scoped
  key > platform key narrowed by forwarded identity headers > headers alone);
  `Scope.CanMutate(target)` is the cross-tenant mutation check.
- `ReverseProxy` / `StripPrefix` / `StripClientIdentityHeaders` / `ActAsTarget`
  (`proxy.go`) — the gateway's proxy layer; injects X-Account-ID/Type/User-ID
  from claims, resolves act-as, propagates traceparent, forwards 3xx verbatim.
- `NewLiveRateLimiter(cfgFn, log)` (`ratelimit.go`) — per-client-IP token
  bucket, live-tuned via the `<svc>.ratelimit_*` TierLive keys; nil-safe `Wrap`.
- `RevocationStore` (`revocation.go`) — Redis iat-cutoff session revocation
  ("log out everywhere"); wire via `WithRevocation` on Auth.
- `CORS`, `CSRF`, `SecurityHeaders` (`cors.go`/`csrf.go`/`security_headers.go`).
- `NewMetrics(service)` (`metrics.go`) — per-service Prometheus registry;
  `Wrap` + `Handler()` + `Registry()` for domain collectors.
- `AttachPprof(mux)` (`pprof.go`) — internal mux ONLY; `/debug/pprof/rates`
  arms block/mutex profiling per-incident.

## Invariants & Gotchas

- **`StripClientIdentityHeaders` MUST run before auth and any header-injecting
  middleware on the gateway** (outermost-but-one in the chain — only
  `SecurityHeaders`, which injects no identity, sits outside it).
  X-Account-ID/Type/User-ID/Publisher-ID are trusted downstream for tenant
  scoping/redaction; without the strip, an unauthenticated pass-through proxy
  forwards a forged `X-Account-Type: staff` verbatim = unauth cross-tenant read
  (a real HIGH finding). `X-Act-As-Account` is deliberately NOT stripped at the
  edge — the gateway consumes and validates it (`auth.CanAccessAccount`), then
  `Del`s it on the upstream forward. See `cmd/gateway/main.go` chain order.
- **Empty signingKey = dev mode**: Auth injects an admin `*` claim for every
  request. Never let that reach a real deployment.
- Revocation and the distributed rate limiter are **fail-OPEN** on Redis errors
  (platform Redis posture); `RevokeUser` write errors ARE returned.
- Rate-limit buckets are **per-pod** (N replicas = N× rate) unless
  `<svc>.ratelimit_distributed` + `WithDistributedBackend` (shared Redis
  fixed-window). Probes/`/metrics`/`/debug/*`/OPTIONS + allowlisted CIDRs
  (private ranges by default) are never limited. Client IP via `pkg/clientip`
  right-anchored trusted-hops parse — never trust raw leftmost XFF.
- `CallerScope` narrowing: the gateway's platform key + forwarded customer
  headers = scoped, not superuser. An unresolved Scope may not mutate.
- `ReverseProxy` strips upstream `Access-Control-*` headers — the gateway's own
  CORS middleware is authoritative (duplicates break the IMA SDK iframe).
- `RequestIsSecure` trusts `X-Forwarded-Proto: https` (Traefik terminates TLS)
  — safe only because it makes cookies MORE restrictive.
- No cross-package deps by design: `RateCounter` / `revocationKV` /
  `APIKeyLookup` are narrow structural interfaces satisfied by `cache.L2Cache`
  / `secrets.Cache` — pkg/middleware never imports pkg/cache (no cycle).
- Body caps are NOT here — enforce request-size limits at the handler.

## Used By

All long-running services (`cmd/*`): gateway mounts the full chain
(SecurityHeaders → Strip → CSRF → tracing → metrics → ratelimit); everything
else at minimum tracing → metrics → CORS (+ ratelimit on public edges).

## Testing

Unit tests in-package use the structural interfaces above as fakes — no Redis or
secrets store needed. Negative-case e2e (forged headers, cross-tenant, unsigned)
live in `tests/e2e/`.

## References

- `docs/PLAN.md` → "Authentication, Roles, and Permissions", "Multi-Tenancy
  Isolation", "Rate Limiting and Back-Pressure", "Auth Infrastructure
  (secrets-as-data, warm-cached)"
- `docs/SECURITY.md` — the enforcement catalog this package implements
