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
- ✅ **Cross-tenant audience-segment isolation** —
  `tenant_isolation_audience_test.go`. Account B can't see A's segment via
  `/v1/api/audiences` (scoped to `claims.AccountID`). Creatives were classified
  and found to have **no** PATCH/DELETE API (create/list only, and create already
  binds to `claims.AccountID`), so there's no cross-tenant creative-mutation
  surface to test.
- ✅ **Role permission scoping** — `role_permissions_test.go`. A read-only
  `viewer` can LIST campaigns but is 403'd on PATCH/DELETE (no
  `campaigns:update`/`:delete`), while the owner can still mutate — proving the
  gate is the role, not a broken endpoint.

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
- ✅ **VAST round-trip (real fill)** — `video_vast_test.go`. Now builds its OWN
  video inventory via new harness helpers `AddVideoPlacement` +
  `CreateVideoCampaign` (format='video' placement with a duration window; a funded
  video line item + video creative with `asset_url`+`duration_seconds`), so it
  actually FILLS and asserts the winning VAST carries a tracked `<Impression>`,
  the creative `<MediaFile>` (.mp4), and quartile beacons through `/v1/t/video`.
  Plus an honest no-fabricated-impression control on an unknown placement.
  **Run GREEN live (2026-07-26) with a real fill.** (Root cause of the earlier
  no-fill: `h.Reset()` TRUNCATEs `placements` with no reseed, so the referenced
  seed placement was gone — fixed by self-provisioning inventory.)
- ✅ **VMAP ad pods (real competitive separation)** — `video_pod_test.go`. Two
  funded video advertisers with EQUAL bids, pod=3 → asserts the pod fills BOTH
  distinct advertisers (not just ≥1) with no repeat. **Run GREEN live** ("2 ads
  across 2 distinct advertisers"). This required FIXING a real product gap the
  test first surfaced: `buildPodVAST` used to only SKIP a repeated advertiser
  post-hoc, so under concentrated demand a pod of 3 filled 1 ad. Now separation
  is enforced at the auction via OpenRTB **badv** (blocked advertiser domains):
  `buildPodVAST` threads the already-picked advertiser domains → SSP sets
  `BidRequest.BAdv` → exchange forwards it → DSP skips any campaign whose
  advertiser domain is blocked. Landed across `pkg/openrtb`, `cmd/ssp`,
  `cmd/dsp`, `cmd/publisher-adserver` (exchange transparently forwards).
- ⬜ **SSAI beaconing** — MODERATE, deferred. The mechanism is fully unit-proven
  (`cmd/ssai/main_test.go`: segment fetch fires the signed beacon + redirects),
  and the harness can query the event (`MediaEventsByTrace`). But an e2e is
  **fill-dependent** (no ad segment to fetch when the video auction doesn't fill)
  and needs trace_id propagation through the manifest→segment→beacon chain that
  isn't verified. An unrun test that silently skips on no-fill would be false
  confidence, so it's held until it can be run to confirm reliable fill.
- ⬜ **`transcoder` / `content-packager` / `prewarm`** — the SSAI conditioning
  chain is unexercised (lower priority; internal, SSAI smoke covers the seam).

## P2 — consent / idempotency edges
- ❌ **Opt-out after win, before impression** — NOT a test, and NOT a bug
  (corrected after an industry-standard review 2026-07-26). Consent binds at the
  bid/serve decision, which is the industry norm (IAB TCF evaluates the TC string
  per node at bid-request time; GPC/CCPA opt-out is *prospective*; impression
  counting for billing is measurement, not a "sale," so it is not consent-gated).
  This platform matches: `publishBehaviour` (`cmd/tracker/main.go:673`) writes a
  user-level `behaviour_signals` row ONLY if `uid` is present, and the uid is
  baked into the beacon URL at serve time ONLY for consented serves
  (`ServeRequest.BehaviourUserID`) — no consent → no uid → no user-level row. The
  retargeting pixel (`main.go:347`) re-evaluates consent live (fresh GPC params).
  Billing is aggregate and correctly ungated. Residual = a seconds-wide race (a
  consented serve whose user opts out before the impression fires writes one
  behaviour row); negligible, standard-acceptable, purged by Level-3 deletion,
  and blocked for future serves. OPTIONAL hardening: a warm-cache opt-out lookup
  on `uid` inside `publishBehaviour` closes the race at one hot-path lookup — not
  a compliance requirement. (My earlier "real code gap" framing was wrong: it
  missed the baked-uid consent mechanism.)
- ⬜ **NATS `Nats-Msg-Id` dedup** — dedup is proven at the tracker/Redis layer
  (`fraud_test.go`) but not that a re-published NATS event is dropped downstream,
  despite exactly-once resting on it.
- ⬜ **Duplicate win-notice** — a double `nurl` must not double-count.

## P2 — input hardening
- ✅ **SQL-injection safety** — `injection_safety_test.go`. A campaign name
  carrying a `DROP TABLE` payload is stored/returned verbatim, the target table
  survives, and the API keeps working — proving the parameterised-queries rule at
  the live boundary.

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
All 11 new tests were **RUN GREEN against the live stack** (2026-07-26). Running
them paid off immediately by catching a **real production bug**:
- **notifications was deaf for 5h+** — all four NATS subscriptions failed once at
  cold boot ("context deadline exceeded" racing NATS/JetStream), the service
  logged `consuming events` anyway and never retried, so the `notifications`
  table stayed empty. Fixed by mirroring the webhooks self-heal (`cmd/notifications`
  now retries failed subjects every 15s until they stick). Same doctrine as
  webhooks (`6d8f4f2`) + the warm caches.
- One test bug fixed too: `dayboundary_test.go` now passes `DATABASE_URL` to the
  `go run` subprocess explicitly (the job hard-exits without it).
