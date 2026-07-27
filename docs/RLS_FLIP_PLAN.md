# Security #77 — the RLS role-flip plan (enumerated)

Flipping the app fleet to the `adtech_app` NOBYPASSRLS role (migration 067) is
the real fix for tenant isolation. The first full-suite flip (2026-07-26) failed
**54 of ~188 e2e tests** — the app was never designed to run under RLS; it relied
on the superuser bypass throughout. This is the enumerated remaining work.

**The rule per query:** a query that legitimately spans tenants uses the
`app.platform_read` hatch (`QueryPlatform`/`QueryRowPlatform`); a query that acts
within one tenant sets that tenant's `app.current_account_id` GUC before running
(a read-only tx for reads, `WithTx` for writes). Over-using the hatch defeats
#77, so most fixes must be tenant-scoped, not hatched.

## Landed this pass (3 root causes, no-ops under the superuser)
- ✅ **Auth/login** (`cmd/gateway/auth_login.go`) — pre-tenant lookup by email →
  platform hatch. Unblocks EVERY login (the flip's hardest blocker).
- ✅ **Campaign PATCH/DELETE lookup** (`cmd/dsp/management.go`) — discover-owner
  lookup → platform hatch (`CanMutate` still authorises the mutation).
- ✅ **Report-job Get/List** (`pkg/reportjobs/store.go`) — tenant-scoped → set the
  caller's GUC.

## Bucket A progress (partial — the pattern is proven, ~13 tests cleared)
Two sub-patterns, both mechanical:
- **Discover-owner lookups** (read a row by id to find its account before the
  caller is authorised) → platform hatch. Fixed: `lookupLineItemAccount(AndStatus)`
  (dsp), `lookupPublisherAccount`/`lookupPlacementAccount` (ssp), moderation
  `Decide` (staff cross-tenant → hatch-resolve then scope to the creative's
  account). The SSP publisher/placement fix alone cleared ~9 `*ViaAPI` tests
  (their setup POSTs a placement).
- **Tenant writes** — set the caller's GUC at the start of the tx. Fixed: gateway
  `CreateDeal`, `CreateDirectLineItem`, quality-controls create.
- ⬜ **STILL OPEN — tenant LIST/read methods** set no GUC, so RLS blanks them
  ("created X not in list"): `ListDeals`, `ListDirectLineItems`,
  `ListQualityControls`, data-providers list, agency-managed list, revshare list,
  and the equivalent read paths across dsp/ssp/gateway. Same one-line fix (set the
  caller's GUC before the SELECT, in a read-only tx held open while scanning).
  This is the bulk of A's remaining sites — mechanical but many.

## STATUS (2026-07-27) — buckets A–E done; ~52 of 54 cleared
The RLS role-flip is essentially complete. All five buckets fixed under the flip
(each commit a no-op under the superuser, verified no regression):
- **A** management CRUD, **B** report-runner worker, **C** notifications +
  datafee, **D** audience/profile ingestion (ingest worker + mappings + segment
  export + SSP membership stamping), **E** billing balance batch-debit.

Reusable primitives landed in `pkg/store/postgres`: `QueryTenantDB`,
`QueryRowTenantDB` (tenant reads), plus per-package `execPlatform`/`claimPlatform`
for worker writes. The three patterns (discover-owner → hatch; tenant → caller
GUC; cross-tenant worker/staff → platform hatch) covered every site.

## STATUS (2026-07-27, pass 2) — all real RLS stragglers CLOSED
The remaining flip failures were investigated and fixed; each was a genuine
cross-tenant read/policy gap (not test pollution, as an earlier note guessed):

- ✅ **Staff onboarding monitor** (`cmd/gateway/onboarding_monitor.go`) — both
  `audience_ingest_jobs` reads (runs list + provider rollup) span every tenant →
  `QueryPlatform`. Was silently empty under the flip.
- ✅ **Staff profile-transparency API** (`cmd/gateway/profiles.go`) — the
  cluster-membership read joins `audience_segments` (RLS) → `QueryPlatform`.
- ✅ **SSP segtax / data-fee warm cache** (`pkg/audience/store/postgres`) — new
  `withPlatformRead` helper; `queryByVisibility` (SegmentsForUser /
  DSPSegmentsForUser bid-request fan-out) and `PublicSegmentMonetization` (the
  taxonomy+fee map) now use it. This unblocked BOTH `TestSegtaxRidesToExternalBidder`
  AND the data-fee stamp.
- ✅ **`data_fee_earnings` RLS policy** (migration 068) — migration 065's hatch
  DO-loop only matched `policyname = 'tenant_isolation'`, so it skipped
  `data_fee_earnings_tenant` (migration 063). The un-hatched policy RAISEd
  "unrecognized configuration parameter" (42704) on the cross-tenant accrual →
  every data-fee earnings insert failed. Rewrote it to 065's NULLIF+hatch shape.
- ✅ **`datafee_test.go`** — `config.value` is `jsonb`, so a float key serialises
  as a JSON string (`"30"`); the old `Sscanf("%f")` choked on the leading quote
  once the reporting pod seeded the row. Strip quotes → `ParseFloat`.
- **NOT RLS (confirmed cleared):** the video/audio tracker 403s were a leaked
  `tracker.signature_validation=true` live-config value; it is now `false`, so
  those tests pass. `TestTopupTenantFlow` + `TestBillingDealTypeFeeModifier` were
  real RLS reads (unscoped balance/deal-type lookups), fixed in pass 1.

### Finalize the flip (deployment, still to do)
1. Persist the app services' `DATABASE_URL` → `adtech_app` in `values.yaml`
   (migrate job stays on the owner/superuser URL).
2. Wire the `adtech_app` password as a SOPS secret (dev value used for the flip
   testing was set out-of-band via `ALTER ROLE`).
3. Un-skip `tests/e2e/rls_test.go` — it can finally enforce.
4. Run the full `make test-e2e` once green end-to-end.

## Superseded — original per-bucket breakdown (kept for history)
## Status (2026-07-26) — buckets A, B, C largely done
Cleared, committed, all no-ops under the superuser:
- **A (management CRUD)** — discover-owner lookups → hatch; tenant reads/writes →
  caller GUC (`postgres.QueryTenantDB` helper); staff cross-tenant writes →
  hatch-resolve-then-scope. dsp/ssp/gateway management CRUD passes under the flip.
- **B (report-runner)** — worker lifecycle + scheduler → platform hatch
  (`execPlatform`). All 3 report tests pass (were 120s timeouts).
- **C (event consumers), partial** — notifications reads (bell) + datafee
  settlement (platform hatch). Notifications test passes.

Three reusable patterns now cover every case: (1) discover-owner → `QueryRowPlatform`;
(2) tenant read/write → caller GUC (`QueryTenantDB` / tenant tx); (3) cross-tenant
worker/staff → platform hatch (`execPlatform` / resolve-then-scope). The
`report_jobs`/`saved_reports`/`data_fee_earnings`/`advertiser_balances` policies
are USING-only, so `platform_read` admits their writes too.

NOT an RLS issue (red herring): `TestVideoTrackerEventReachesReporting` /
`...Audio...` return 403 **under the superuser too** — an earlier strict-mode
test left `tracker.signature_validation=true` in live config and didn't reset it
(test-isolation leak). Fix belongs in that test's cleanup, not #77.

## Remaining buckets (root cause → tests it blocks)

### A. Management CRUD store methods don't set the tenant GUC (biggest)
DSP/SSP/gateway create/edit/read for campaigns, creatives, deals, placements,
targeting, freqcaps, revshare, bid-modifiers, data-providers, direct-line-items,
agency act-as. ~18 direct failures, and it CASCADES into bucket E (auction tests
seed via these APIs). Fix: every management store method runs its read/write
under `WithTx`/a tenant tx with the caller's account. Tests: `*ViaAPI`
(BidModifiers, CampaignCreativeAttach, CampaignFreqCap, CampaignTargeting,
DaypartFloors, DealDepth, DirectLineItems, OSKeywordTargeting, PlacementFloor,
PlacementVideoConfig, Revshare, SegmentTargeting, TimeOfDay), TargetingSegment
Include, TargetingDSPPrivateSegment, AgencyActAsViaAPI, DataProviderRegistry,
OnboardingJourney.

### B. Background workers do cross-tenant claims (report-runner)
`report-runner` `ClaimOne`/`ExtendLease`/`MarkDone`/`Expired` scan `report_jobs`
across ALL tenants — RLS blanks them so jobs never process → timeouts. Fix:
platform hatch to claim, then set the claimed job's account GUC for its work.
Tests: ReportJobsEndToEnd (121s), ScheduledReportBecomesJob (120s),
ReportWebhookDelivery (60s).

### C. Event consumers insert per-account rows without the account GUC
`notifications`, `reporting` ingestion, `datafee` consume NATS events carrying an
account_id and INSERT/UPDATE per-account rows. Fix: set the event's account GUC
before the write (platform for genuinely cross-tenant aggregates). Tests:
CampaignStateNotificationReachesPortal (20s), VideoTrackerEventReachesReporting,
AudioTrackerEventReachesReporting, DataFeePaysOwnerOnExternalWin.

### D. Audience / profile ingestion + reads
Gateway upload, pipeline drop-zone poller, profile-builder, segment reads. Fix:
tenant tx from the upload/segment account; the poller sets each file's provider/
account. Tests: Audience* (UploadWritesMembers, Freshness, BidModifier, Reject,
CustomMapping, IngestHistory, IngestEmail, PGPUpload), Profile* (OnboardingCSV,
OnboardingDropZone 90s, APIAndMonitor), OnboardingRetentionSweep (45s),
SegmentExportJob, DerivedRuleSegments, BehaviouralRuleFlipsTargeting,
RetargetingPixel, PrivacyDeletionPropagation.

### E. Auction/routing/billing — mostly DOWNSTREAM of A
Fast fails (0.05s): the harness seeds campaigns/deals via the bucket-A APIs, which
fail under RLS → no inventory → the auction asserts nothing. Fixing A should clear
most. Residual billing writes (balance/ledger) need the account GUC. Tests:
CompetitiveB1/B2/B4/B5/B7, CompetitiveG2, RoutingKnobsLive, SmartRoutingSkips,
SegtaxRidesToExternalBidder, BillingDealTypeFeeModifier, BalanceExhaustion,
TopupTenantFlow.

## Method (per bucket, until green)
1. Fix a bucket's store methods/consumers to establish tenant context.
2. Flip → run the full suite → the bucket's tests (and its cascade) go green.
3. Repeat A→E. A is the highest-leverage (clears its own ~18 + much of E).
4. When green: persist the flip in `values.yaml`, wire the `adtech_app` password
   as a SOPS secret, and un-skip `tests/e2e/rls_test.go`.

## Honest scope
This is a **large, multi-service refactor** (~5 architectural patterns, most
services), not a few fixes — the prior "pre-flip audit clean" claim was wrong.
Each fix is small and local, but there are many, and each carries a tenant-vs-
platform security judgment. Best done bucket-by-bucket with a flip+full-suite
gate between each.
