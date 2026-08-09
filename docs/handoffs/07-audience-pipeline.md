# Session: understand, judge, then stress the audience pipeline

> **PROGRESS (2026-08-09)** — Phases 1+2 DONE (6edab56, 67a56f5, 99c2358):
> themed two-tier world shipped + live-verified, `docs/AUDIENCE-PIPELINE.md`
> explainer written, targeting-data-flow + cache-freshness diagrams
> de-drifted, and a latent bug fixed — the in-cluster hourly profile-builder
> had been a SILENT NO-OP (RLS-blanked reads under the app role) since the
> DB-role flip. Phase 2 also complete: TTL-expiry (43s scrub via purge
> trigger) + duplicate-ingest (idempotent, zero changelog churn) traced
> live; the disruptive trio ran with operator approval — drainer down 3.5m
> (2s recovery, no loss), FLUSHDB (233s stale window, new enrolls kept
> flowing), NATS outage (no deaf-on-boot latch). Fixes shipped: zero-work
> WARN in the builder + AudienceCacheWriterAbsent/AudienceChangelogLag
> Prometheus alerts. Full fault table in docs/AUDIENCE-PIPELINE.md.
> Phase 3 (density + cadence) not started — next session.

## Why
The audience pipeline was built incrementally across many sessions and the
operator doesn't currently hold a mental model of it. Before tuning
anything: LEARN it end-to-end, then JUDGE it (simple? fault-tolerant?
understandable? observable?), then LOAD it at production density and choose
recompute cadences deliberately. Teaching is a first-class deliverable of
this session — explain each stage as it's visited, not just audit it.

## Phase 1 — guided walkthrough (produce an explainer, not just findings)
Trace ONE datum through each path, live on the stack, showing the actual
rows/keys/messages at every hop:

- **Slow path (uploads)**: file → gateway/dropzone staging → single
  `audience_ingest_jobs` queue → pkg/ingest.Processor (PGP, field
  mappings, strict all-or-nothing validation) → `audience_segment_members`
  (provider_id/data_party lineage stamped).
- **Slow path (behaviour rules)**: tracker events → ClickHouse
  `behaviour_signals` → hourly batch-conductor chain (completion-ordered
  CODE: checkpoint→compact→rollups→profile-builder→privacy; `batch_runs`
  table + staff page) → cmd/profile-builder rule eval → membership writes.
- **Fast path**: `/v1/t/rt` site_visit → cmd/audience-rt (1 replica) →
  same `audience_segment_members` within seconds; purchase suppression;
  min_count<=1 rules ONLY — understand and document WHY that boundary.
- **Serving fan-in (the clever bit)**: DB trigger (mig 078) →
  `audience_membership_changelog` outbox → SINGLE drainer in cmd/pipeline
  (3s poll, atomic SADD/SREM to `audience:set:*`, 5m reconcile with
  prev-keys tombstones, Redis-sourced watermark) → DSP/SSP read-only
  preloaders (SMEMBERS → in-process) → bid-loop lookups (never network).
- **Exits**: member TTL aging (mig 073 read-side filter), GDPR purge,
  composite/lookalike derivation, segtax attach at SSP (consent-gated).

Deliverable: `docs/AUDIENCE-PIPELINE.md` explainer + update the matching
D2 diagram (docs/diagrams — check the "Update when" column) if the walk
finds drift. Memory topics to pre-read: project_audience_preloader_delta,
project_realtime_retargeting, project_unified_ingestion,
project_audience_segments_state, project_batch_conductor.

## Phase 2 — judgement pass (the operator decides, with evidence)
For each stage answer: what happens when it's down / slow / doubly-
delivered? Kill things live and watch (chaos-style, on a disposable
world): pipeline pod down mid-drain (changelog backlog + resume), Redis
FLUSHDB (reconcile rebuild + watermark desync gotcha), NATS outage
(audience-rt deaf-on-boot rule!), duplicate ingest job, TTL-expiry vs
reconcile-tombstone interplay. Verdicts: is the single-drainer design
sound; is anything needlessly complex or duplicated; where are the silent
failure modes; is observability adequate (can the operator SEE backlog,
drain lag, membership counts per segment today?).

## Design decision (operator, 2026-08-07): a THEMED, two-tier world
Local segments/personas/campaigns get memorable, human-checkable themes:
a readable core of ~6-10 segments (dog-lovers, cat-lovers, coffee-snobs,
...), simulator personas whose BEHAVIOUR coherently earns membership (a
"dog person" browses dog content → behaviour signals → the hourly rule
enrolls them — Phase 1's walkthrough watches a dog-lover get CREATED),
and campaigns that target them ("Premium Dog Food Co" → dog-lovers). DSPs
may take themed skins ("Barkstream DSP") but the four competitor
PERSONALITIES (slowpoke/deadbeat/healthy/coinflip) are load-bearing for
the SmartRouter bench + perf baselines and MUST keep their latency/
bid-rate semantics. Verification then reads like English: the dog-food ad
wins on the dog-blog page for dog personas and never shows to pure cat
people. Density testing (Phase 3) bulk-adds SYNTHETIC dummy segments/
memberships via a seed flag AROUND the readable core — perf numbers
without sacrificing comprehension.

## Phase 3 — density + cadence (from the perf sessions' thread)
- Seed a segment-dense world: O(100) segments, O(100k-1M) memberships,
  a slice of big-world campaigns with audience predicates + bid modifiers
  (bigworld.go currently targets geo+device ONLY — auction-side audience
  work is unmeasured).
- Measure: profile-builder runtime at CH volume, changelog drain
  throughput, Redis set memory, preloader RSS per DSP pod, DSP audience
  phase p95 (ledger column; ~15ms at 110rps on thin data), and
  signal→bid-eligible latency per path (pixel seconds / hourly rule).
- Then choose cadences ON PURPOSE: is hourly right for frequency rules
  (or add a 15m tier)? changelog_poll_interval 3s? reconcile 5m? Record
  the chosen policy + rationale in the explainer.

## Protocol notes
Bench comparisons follow /perf-loadtest + docs/perf/BASELINE (fanout p95
~46-53ms @110 on the trimmed stack). Pre-run: jetstream pending ≈ 0;
after any VM restart, ~5min simulator warm (`go run ./cmd/simulator run
--duration 300s --rps 30` — macOS has NO `timeout`). Code-only changes
need explicit rollout restarts (same-tag images don't roll on stack-up).
