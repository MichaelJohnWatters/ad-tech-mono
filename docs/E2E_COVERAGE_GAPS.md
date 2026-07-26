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
- ✅ **`dayboundary`** (`cmd/dayboundary`) — `dayboundary_test.go`. Drives the
  job as the CronJob does (`go run ./cmd/dayboundary --date …`) and proves a
  campaign whose flight has ended cascades to `ended` (IO active→ended, line
  item live→ended), and that a re-run is idempotent. Follow-up: the
  `ActivateFlights` direction (draft IO → active) needs a direct draft-IO
  fixture since the API creates IOs `active`.
- ✅ **`notifications` (`:8096`)** — `notifications_test.go`. Proves the full
  chain: a campaign live→paused transition → `SubjectCampaignStateChanged` → the
  notifications consumer → the `notifications` table → the tenant-scoped portal
  bell API (list + unread count + mark-read).
- ⬜ **`appadstxt`** — daily app-ads.txt crawler; no test it crawls + upserts.
  Feasible but needs a host-reachable fake-TLS server + a publisher
  developer-domain seed helper (`harness.HostReachableServer` exists); deferred.
- ✅ **CSRF cross-site block** — `csrf_test.go`. A cross-site, cookie-authed POST
  → 403; absent-Origin / same-origin / safe-method requests pass (the exemptions
  that keep legitimate + non-browser traffic working).
- ✅ **Session-cookie tamper** — `session_auth_test.go`. A valid session is 200;
  a tampered signature or a garbage cookie → 401 (the JWT signature check).

## P2 — rich-media depth (Phase 9 is orchestration-tested, not format-tested)
- ✅ **VAST round-trip** — `video_vast_test.go`. GETs the live
  `/v1/pubad/video/vast` for a seeded video placement and asserts a well-formed
  VAST 4.2 doc; on fill: a tracked `<Impression>` + `<MediaFile>` + quartile
  beacons through `/v1/t/video`; on no-fill: an honest Ad-less doc with NO
  fabricated `<Impression>` (real-data-only rule).
- ✅ **VMAP ad pods** — `video_pod_test.go`. `?pod=3` → asserts the core
  competitive-separation invariant (no advertiser repeated across the pod, which
  holds for any fill count so it's deterministic) and pod ordering (sequence
  attributes when >1 ad fills).
- ⬜ **SSAI beaconing** — `TestSSAIManifestStitched` proves a manifest returns;
  nothing asserts server-side quartile beacons reach the tracker/reporting, or
  that a no-bid break yields slate.
- ⬜ **`transcoder` / `content-packager` / `prewarm`** — the SSAI conditioning
  chain is unexercised (lower priority; internal, SSAI smoke covers the seam).

## P2 — consent / idempotency edges
- ❌ **Opt-out after win, before impression** — NOT a test. Investigated and
  confirmed the tracker does NOT re-check opt-out at impression time (opt-out is
  enforced only at DSP bid time — `cmd/dsp/main.go`; the impression handler in
  `cmd/tracker` validates sig/expiry/fraud/dedup then publishes unconditionally).
  So "a late impression post-opt-out doesn't bill" is not current behavior — a
  test would assert fiction. Recorded as a design observation, not a bug: consent
  is a bid/serve-time gate here, and an already-served impression completing is a
  seconds-wide race. Revisit only if the privacy model requires tracker-side
  opt-out enforcement.
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
