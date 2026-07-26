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
