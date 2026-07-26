# E2E coverage gaps (review 2026-07-26)

The suite is 74 files / 188 top-level `Test` funcs (`go test -tags=e2e`). It is
strong on happy-path serving, billing, targeting, deals, and audience flows. This
is the prioritized list of what it does **not** cover, from a gap review that
cross-checked the feature surface (`cmd/*`, `docs/PLAN.md`) against the tests and
verified each claim against the actual files (several sub-agent "gaps" were
false — see "Not gaps" at the bottom).

Status key: ✅ done · ⬜ todo.

## P0 — security / correctness, small, no exclusive stack needed
- ✅ **Cross-tenant campaign API isolation** — `tenant_isolation_api_test.go`.
  Advertiser B cannot PATCH/DELETE/list advertiser A's campaigns (locks in the
  `middleware.CallerScope.CanMutate` IDOR fix in `cmd/dsp/management.go`, which
  had **no** regression test). Includes an unauthenticated-mutation 401 and an
  owner-can-still-mutate sanity case.
- ✅ **Malformed input at the edge** — `malformed_input_test.go`. Invalid JSON to
  the external Prebid OpenRTB ingress → 400; wrong method → 405/503; campaign
  create missing required `name` → 400; non-UUID id in a path → 4xx not a leaked
  5xx Postgres cast error.
- ⬜ **Cross-tenant isolation for creatives + audiences** — extend the same
  pattern to `/v1/api/creatives/{id}` (PATCH/DELETE) and audience list/export.
  (Campaigns are the proven-protected path; creatives/audiences need their own
  handler classified first — a tenant-scoped read wrongly treated as platform
  would be a real hole.)

## P1 — untested built services / jobs
- ⬜ **`dayboundary`** (`cmd/dayboundary`) — the nightly flight-transition
  cascade. Built + unit-tested, but no e2e proves a campaign actually flips
  in/out of flight at the boundary. High blast radius if it silently no-ops.
- ⬜ **`notifications` (`:8096`)** — consumes budget/balance/campaign/report NATS
  events, writes per-account portal rows. Zero coverage. (The "notification"
  hits in the suite are auction win/loss notices — a different thing.)
- ⬜ **`appadstxt`** — daily app-ads.txt crawler; no test it crawls + upserts.
- ⬜ **CSRF + session-tamper negatives** — `pkg/middleware/csrf.go` and the
  `Secure`-cookie logic exist but nothing asserts a state-changing POST without a
  valid CSRF token, or with a tampered session cookie, is rejected.

## P2 — rich-media depth (Phase 9 is orchestration-tested, not format-tested)
- ⬜ **VAST/VMAP round-trip** — a real video auction returning VAST 4.2 with
  signed quartile beacons; ad-pod competitive separation (no advertiser twice).
  Today only placement *config* CRUD + a manifest smoke test exist; VAST
  generation is unit-tested only (`cmd/publisher-adserver`).
- ⬜ **SSAI beaconing** — `TestSSAIManifestStitched` proves a manifest returns;
  nothing asserts server-side quartile beacons reach the tracker/reporting, or
  that a no-bid break yields slate.
- ⬜ **`transcoder` / `content-packager` / `prewarm`** — the SSAI conditioning
  chain is unexercised (lower priority; internal, SSAI smoke covers the seam).

## P2 — consent / idempotency edges
- ⬜ **Opt-out after win, before impression** — a late impression post-opt-out
  must not bill.
- ⬜ **NATS `Nats-Msg-Id` dedup** — dedup is proven at the tracker/Redis layer
  (`fraud_test.go`) but not that a re-published NATS event is dropped downstream,
  despite exactly-once resting on it.
- ⬜ **Duplicate win-notice** — a double `nurl` must not double-count.

## Not gaps (sub-agent claims that were false on inspection)
- Cross-tenant isolation **is** already tested for report-jobs, topup,
  data-providers, direct-line-items, profile-payoff. The gap was campaigns/
  creatives/audiences specifically.
- `cleanroom` and `optimise` are empty `.gitkeep` stubs — unbuilt, so "no test"
  is correct, not a gap.
- `privacy-delete` and `webhooks` are covered (`batch_conductor_test`,
  `report_webhook_test`).
- `rls_test.go` is intentionally skipped (dev role is BYPASSRLS superuser); RLS
  enforcement is proven at the integration layer
  (`pkg/store/postgres/rls_platform_read_integration_test.go`). See
  `docs/SECURITY_HARDENING.md` §3.

## Note
The two ✅ tests are committed vet-clean under `-tags=e2e` but were **not** run
against the live stack this session (it is shared with another active context and
both tests call `h.Reset`, a destructive reseed). They run in CI / a full
`make test-e2e` on an exclusive stack.
