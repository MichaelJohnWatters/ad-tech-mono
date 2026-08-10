# Security Enforcement Consistency Audit — prompt

Hand this prompt to a fresh reviewing context (or an agent) to verify that every
security control in [`SECURITY.md`](SECURITY.md) is not only implemented but
**applied everywhere it is meant to be** — the goal is to catch OMISSIONS (a
forgotten permission gate, an unscoped query, an unsigned endpoint, a money-write
without idempotency, an internal identifier leaking on a public/customer
response), not to re-list the controls that already exist.

---

You are a senior application-security reviewer. This Go monorepo is a
programmatic ad platform with a documented set of security-enforcement
mechanisms (see `docs/SECURITY.md`). Your job is NOT to rediscover them — it is
to verify each is (a) implemented correctly and (b) **applied everywhere it is
supposed to be, with no gaps**. The primary deliverable is a list of PLACES WHERE
A REQUIRED MECHANISM IS MISSING.

Be adversarial. Assume something was forgotten. A "looks fine" pass is a failure —
your value is the ONE handler that skipped the gate. Verify by READING the code,
not by trusting names or comments. Do not modify any files. Cite every finding as
`file:line`.

## Method (in order)
1. **Enumerate the expected-use sites** for each mechanism below (e.g. every
   `/v1/api/*` route in `cmd/gateway/main.go`; every store method touching a
   tenant table; every tracker event endpoint; every NATS consumer of a money
   event; every response struct serialized to customers/public).
2. **Check each site** against the rule for its category. Where the rule is NOT
   satisfied, that's a finding.
3. Prefer a coverage matrix (site × mechanism × present/absent) over prose. Fan
   out with parallel searches if you can.

## Mechanisms, required-use rule, and the "smell" to grep for

### 1. RBAC / permission gates
- **Rule:** every non-public `/v1/api/*` (and `/v1/dsp/*`, `/v1/ssp/*`) route must
  enforce a permission — `middleware.RequirePermission(...)` at registration OR an
  in-handler `can(claims, "resource:action")` for EVERY method it serves. A
  handler that checks `claims == nil` but never calls `can()`/`HasPermission` is a
  gap. Multi-method handlers must gate each method.
- **Find it:** list `mux.Handle`/`mux.HandleFunc` in `cmd/gateway/main.go`; open
  each handler and confirm a perm check on every branch. Grep handlers for
  `ClaimsFromContext` with no following `can(`/`RequirePermission`/`HasPermission`.
- **Also:** owner-only (`account:close`, `account:export`) and staff-only
  (`support:read/update`, `incidents:*`, `ops:*`, `config:update`, `moderation:*`)
  perms must appear ONLY on the intended roles in `pkg/auth/auth.go`.

### 2. Multi-tenant isolation (RLS + scoping)
- **Rule:** every read/write of a tenant-scoped table must be reached under the
  tenant GUC (`app.current_account_id`) OR filtered by `account_id = $caller` OR a
  legitimately staff/loader path using the platform hatch (`app.platform_read`). A
  **write** to an RLS table from a bare pool connection (no GUC) fails RLS WITH
  CHECK — hunt these. A staff cross-tenant read via the hatch that isn't gated by a
  staff permission is a gap.
- **Find it:** grep for `db.Query`/`db.Exec`/`QueryRowContext` directly on the pool
  (not inside a `withTenant`/`tenantTx`/`SetTenantContext`/`QueryPlatform` helper)
  in `cmd/gateway/*.go` and `pkg/*/postgres/*.go`; confirm scoping for each.
- **New tables:** for EVERY `CREATE TABLE` in `migrations/` with an `account_id`
  column, confirm a matching `ENABLE ROW LEVEL SECURITY` + `tenant_isolation`
  policy of the `NULLIF(current_setting('app.current_account_id',true),'')::UUID OR
  current_setting('app.platform_read',true)='on'` shape. A tenant-shaped table with
  NO RLS policy is HIGH. Confirm which no-RLS tables are intentionally
  platform-global.
- **Linchpin:** verify each env's `DATABASE_URL` (k8s/helm `values-*.yaml`) uses
  the `adtech_app` (`NOBYPASSRLS`) role, not a superuser — RLS is inert otherwise.
- **Trusted headers:** `X-Act-As-Account` must be validated via `CanAccessAccount`
  before use; `X-Account-ID`/`X-Publisher-ID` trusted only when gateway-injected.

### 3. Cryptographic signing / anti-spoofing
- **Rule:** every tracker event endpoint (imp/click/view/media/conv) must validate
  its HMAC (blank attribution / 403 per strict-mode gate); every outbound tracking
  URL built through `SignURL` (never hand-concatenated); every webhook delivery
  HMAC-signed; conversion postbacks validate the per-advertiser key; data-fee /
  marketplace settlement bills the exchange-bound `SettlementSeat`, never
  `SeatBid.Seat`.
- **Find it:** grep `pkg/adserving/macros.go` + adserver for URLs built without
  `SignURL`; grep `cmd/tracker/` for event handlers not routed through the
  signature gate (`mediagate.go`/`ValidateSignatureAny`); grep settlement/reporting
  for `SeatBid.Seat` where `SettlementSeat` is intended.

### 4. Privacy & consent
- **Rule:** every path publishing behaviour/identity or expressing user data to
  external bidders must gate on `privacy.Evaluate(...).Personalise` (or the
  `dsp_private` visibility check). An un-gated `publishBehaviour` / identity-observe
  / segment-to-`user.data` path is a gap.
- **Find it:** grep `publishBehaviour`, identity `observed` publishes, and
  `user.data`/segment expression in ssp/exchange/tracker; confirm a consent
  decision precedes each.

### 5. Idempotency / replay (money + events)
- **Rule:** every money-write (ledger/spend/settlement/data-fee/surcharge/invoice/
  payout/adjustment) must be replay-safe (PK-claim `ON CONFLICT DO NOTHING
  RETURNING` or a dedup key); every NATS consumer of a money/business event must
  dedup (`Nats-Msg-Id`/Redis SetNX/business key).
- **Find it:** grep `INSERT INTO ledger_entries`/`advertiser_balances`/`*_earnings`
  /`invoices`/`adjustments` and confirm a conflict/dedup guard; list NATS
  subscribers and confirm each money-relevant one is wrapped in the idempotent
  handler.

### 6. Information leaks in responses
- **Rule:** structs serialized on PUBLIC endpoints (e.g. `/v1/api/status`) or to
  CUSTOMERS must not carry internal identifiers — staff JWT subjects (`created_by`,
  `author_id`, `assigned_to`), internal service URLs, or other tenants' data. The
  UI hiding a field does NOT count; check the raw JSON the handler emits.
- **Find it:** for each public/customer response type, list its serialized fields
  (json tags) and flag any internal id not redacted on that path. Check
  `pkg/statuspage`, `pkg/support`, marketplace responses, the account-export zip.

### 7. Secrets handling
- **Rule:** secret values never logged in full (only truncated), never returned
  unmasked on a GET after creation, stored through the AES-GCM cipher path. Grep
  for `log.*key`/`log.*secret`/`log.*token` emitting full values, and GET handlers
  returning raw secret/key values.

### 8. Audit coverage
- **Rule:** state-changing, cross-tenant, staff, or money actions should call
  `audit.Log(...)`. Find mutating handlers (POST/PATCH/DELETE, esp. staff/money)
  with no audit call.

### 9. Input / SQL safety
- **Rule:** no string-concatenated SQL. Grep `fmt.Sprintf(`/`+` inside query
  strings passed to `Query`/`Exec`; confirm enum inputs are validated against the
  migration CHECK constraints.

### 10. Rate limiting / transport
- Confirm public/browser-facing services (gateway, tracker, adserver, ssp) mount
  the rate-limit middleware; flag any that don't. Confirm free-text fields have
  length bounds. Confirm gRPC is used only on owned internal edges.

## Output
1. **Coverage matrix** — rows = enforcement sites (routes / store methods / event
   consumers / response types), columns = the relevant mechanism, cell = OK /
   MISSING / N/A + a one-line note.
2. **Findings** — each gap as `{severity: high|med|low, category, file:line,
   what's missing, why it matters, the fix}`. Sort by severity.
3. **Verdict per category** (OK / GAPS FOUND) + the most important 3 fixes.

Rules: read the actual code; do not trust comments/names; do not modify files;
cite `file:line`. Verify a suspected gap by tracing the request path end-to-end,
not pattern-matching alone.
