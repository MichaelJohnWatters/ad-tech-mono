# Session: product gaps — Privacy Sandbox ARA + PLAN phases 10-12

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
