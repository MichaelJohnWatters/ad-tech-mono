# Session: product gaps — Privacy Sandbox ARA + PLAN phases 10-12 + DPA

## Candidate epic (added 2026-08-09): Dynamic Product Ads (catalog creatives)
Born from the retargeting chase demo (docs/demos/RETARGETING-CHASE-DEMO.md
+ memory: project_audience_pipeline_walkthrough). Today the WHO is precise
(cart-abandoner segments, person+household, durable burn-list suppression)
but the WHAT is a static campaign creative. Real cart retargeting
(Criteo/Meta-DPA-style) is catalog-driven:

1. **Product catalog entity** — advertiser-scoped product feed (SKU, title,
   image URL, price, availability, product URL). Ingest rides the EXISTING
   unified ingestion framework (pkg/ingest + audience_ingest_jobs pattern —
   ADR 0007/0008: queue, strict validation, field mappings, PGP); this is
   a second feed type, not new machinery.
2. **SKU-aware pixel** — /v1/t/rt gains product params (sku=, or skus= CSV)
   the way it carries tag=; enrollment remembers user↔SKUs (new table or
   member metadata) with the same TTL semantics as membership.
3. **Dynamic creative template** — a new creative format the adserver
   assembles at render: template + catalog lookup for the user's carted
   SKUs (macro substitution next to the existing beacon macros / native
   asset assembly). Fallback to the campaign's static creative when no SKU
   context exists.
4. **Per-product suppression + cross-sell** — the purchase burn-list
   (migration 082) gains an optional SKU dimension: bought SKU-A → stop
   showing SKU-A, optionally rotate to complementary catalog items. This is
   where retargeting revenue actually lives.

Rough shape: comparable to the attribution epic. Natural order 1→2→3→4;
each slice independently demoable on the chase demo (the shop demoadv gets
a product grid, the chase ad starts showing THE actual carted product).

## Context
Platform phases 1-9 are built (memory: project_phase9_status,
project_mock_audit — some async/cron shells noted). The ONE named feature
gap in attribution is Phase 4: Privacy Sandbox ARA (Attribution Reporting
API) — docs/attribution-plan.md has the phase breakdown; everything else
(click-through, view-through, cross-device, multi-touch, per-advertiser
signing keys) is SHIPPED + e2e-green (memory: project_attribution).

## Goal
1. Read docs/PLAN.md "Build Status & Outstanding Work" + the phase 10-12
   sections — inventory what remains (unknown to memory; reconcile).
2. Propose a prioritized plan for the remainder (ARA vs phases 10-12 vs
   the mock-audit shells) — get user sign-off on scope before building.
3. Build the agreed slice using the house doctrine: e2e against the full
   local stack (make stack-up; NEVER partial stacks —
   feedback_e2e_full_stack), seed via profiles, verify-pipeline skill for
   losslessness.

## Read first
docs/PLAN.md (source of truth), docs/attribution-plan.md, memory index
(MEMORY.md) — attribution, phase9, mock-audit entries.
