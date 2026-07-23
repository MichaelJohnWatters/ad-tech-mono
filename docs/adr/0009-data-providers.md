# ADR 0009 — First-class data providers (the DMP foundation)

**Status:** Accepted — all 4 phases shipped + verified on the live stack (2026-07-23)
**Extends:** ADR 0007 (unified audience ingestion) + ADR 0008 (secure + mappable
ingestion). Promotes the free-text `provider` string and the per-tenant
`audience_mappings` / platform PGP key into a first-class **data provider**
entity — the noun a DMP is built around — while explicitly deferring the DMP
*business* layer (marketplace, rate cards, revenue share).

## Context

This platform's stated goal is to be a full-stack SSP **+ DSP + DMP**, traceable
end-to-end. Audience ingestion (ADR 0007/0008) gave us a durable queue, one
processor, strict validation, PGP decrypt, custom mappings, and completion
emails. But the thing a DMP actually organises itself around — **who the data
came from** — is still second-class:

- `audience_ingest_jobs.provider` is a free-text string (`acme`), NULL for API
  uploads. It names a supplier but carries nothing about them.
- **Party** (1st/2nd/3rd) is implicit: re-derived ad hoc from a licence string
  (`defaultAccess` → `purchased:{provider}` | `first_party`) and the presence of
  a provider. There is no column you can report, target, or apply a GDPR policy
  on.
- **Field mappings** (`audience_mappings`, ADR 0008) are per-*tenant*, not
  per-*provider* — yet "a provider sends files in its own format" is a per-
  provider fact.
- The **PGP** expectation is global: we can decrypt, but we can't say "provider X
  is contracted to encrypt; reject cleartext from them."
- **Notify emails** live only on the job row.

### The DMP capability checklist (where we stand)

| DMP capability | Today | This ADR |
|---|---|---|
| Provider/source onboarding | free-text string + per-tenant mappings | **first-class `data_providers`** |
| Identity onboarding + match rate | ✅ processor → match_rate | unchanged |
| Party classification (1st/2nd/3rd) | implicit in a licence string | **explicit derived `data_party`** |
| Licence / permitted-use | `Access` = `purchased:{p}` / `first_party` | resolved from provider default |
| Segment taxonomy (IAB) | ✅ `taxonomy_categories` | unchanged |
| Activation / syndication to DSPs | ✅ `visibility` public/dsp_private | unchanged |
| Consent / privacy / purge | ✅ consent + GDPR purge | party rides along for policy |
| Data marketplace + rate cards + rev-share | ❌ | **deferred — seams only** |
| Overlap / reach / freshness reports | partial (`expires_at`) | **deferred — seams only** |

## Decision

**Introduce a tenant-scoped `data_providers` entity. Everything provider-shaped
hangs off it. Party is set once per provider and stamped per file. Every link is
an additive, nullable FK — the ADR-0008 tables are enriched, never torn up.**

1. **`data_providers`** (Postgres, RLS tenant isolation like `audience_mappings`):
   `id, account_id, name, kind (crm|dmp|cdp|agency|other), default_party
   (first|second|third), default_licence (first_party|purchased|barter),
   default_id_type, encryption_expected bool, notify_emails text[], status,
   created_at, updated_at`. Deferred monetization columns are added **nullable and
   unused**: `scope, cost_cpm_micros, taxonomy_vendor_id, contract_ref`.

2. **Additive links (all nullable — providers are opt-in):**
   - `audience_mappings.provider_id` → a mapping may belong to a provider or stay
     a standalone tenant format (a provider can have several named feeds; keeping
     mappings a child is more flexible than folding 1:1).
   - `audience_ingest_jobs.provider_id` (keeps the legacy `provider` text for
     drop-zone display).
   - `audience_segments.provider_id`.

3. **Derived `data_party` on `audience_segments`** (+ the `ProfileSignalEvent`),
   resolved at ingest: **upload override → provider `default_party` →
   `first_party`**. Set once per provider, stamped per file — never asked per
   upload (uploaders get it wrong; it's redundant with the licence). This retires
   the `Access`-string parsing in `defaultAccess`/`segmentSource`.

4. **PGP key stays platform-wide; the provider carries `encryption_expected`.**
   One platform decrypt key is simpler and standard (one public key to publish).
   What's per-provider is the *expectation*: an `encryption_expected` provider's
   cleartext file is rejected (Phase 4).

5. **`data_party` + `provider_id` flow into the ClickHouse signals table** so
   reporting can group match-rate / spend **by provider, by party, by licence** —
   the "cool queries later" payoff. Additive columns, NULL-backfilled.

6. **Providers are tenant-scoped now, with a `scope` seam for global later.** In
   this repo every store method is tenant-filtered (the CRITICAL rule) and uploads
   are per-tenant (an advertiser's own CRM/CDP). A true DMP also runs a *platform*
   catalogue of 3rd-party sources every tenant can subscribe to — that is the
   monetization layer. The nullable `scope` (`tenant`|`platform`) column + a
   future `provider_subscriptions` table add global providers **without reshaping
   anything**.

## Consequences

**Positive**
- The DMP's spine exists: a provider is an entity with a party, a licence, a
  format, an encryption contract, and notification defaults.
- Reporting/targeting/GDPR can key on `data_party` and `provider_id` instead of
  string-parsing a licence.
- ADR-0008 mappings/PGP/emails keep working unchanged (every link is nullable).
- Clean seams for the monetization layer without building it now.

**Negative / cost (accepted)**
- One ClickHouse schema change (Phase 2) — additive + re-export of affected
  hours; the only non-Postgres migration.
- A provider is another thing to model in the portal; kept minimal (a provider is
  optional — plain uploads still work with no provider).

## Explicit non-goals (deferred; seams left)

Data-marketplace discovery, per-provider **rate cards / CPM billing for data**,
**revenue-share payouts** to providers, taxonomy syndication, and overlap/reach
reports. All bolt onto `provider_id` + the nullable `scope`/`cost` columns later
— *because* the entity exists now. We build the DMP's nouns; we defer its verbs
until traffic justifies them.

## Migration plan (phased — each shippable + reversible)

1. **Entity.** Migration 060 (`data_providers` + nullable `provider_id` FKs +
   `data_party` on segments); `pkg/dataproviders` (Store, Postgres+memory, RLS,
   party-derivation helper) + tests. No behaviour change.
2. **Resolution + stamping.** Processor resolves party/licence/notify/mapping
   from `provider_id` when set; stamps `data_party` + `provider_id` on the segment
   + `ProfileSignalEvent`; ClickHouse signals columns + reporting group-by.
3. **API + portal.** `/v1/api/audiences/providers` CRUD; provider selector on the
   upload form; provider + party in upload history and the segment list; mappings
   optionally created under a provider; openapi.
4. **Enforcement + monitor.** `encryption_expected` rejects cleartext;
   onboarding-monitor + segment views surface provider/party; optional staff
   Data Providers overview.

## Open questions (resolved)

- **Global vs tenant providers?** Tenant now (matches RLS + how uploads work);
  `scope` seam for platform-global providers with the monetization layer.
- **Fold mappings into the provider?** No — keep `audience_mappings` a child via a
  nullable `provider_id`, so a provider can have several feeds and ad-hoc tenant
  mappings still work.
- **Per-provider PGP keys?** No — one platform decrypt key; the provider carries
  only the `encryption_expected` contract flag.
