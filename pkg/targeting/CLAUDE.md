# pkg/targeting - Targeting Rule Evaluation Engine

Pure in-memory evaluation of whether a bid request matches a line item's
targeting, plus bid modifiers, contextual (IAB) page classification, and
inventory quality scoring. Sits inside the DSP's per-campaign bid loop.

## Key Entry Points

- `Evaluate(rules Rules, req Request) Result` (`targeting.go`) — inclusion/exclusion
  match. Inclusions: AND across dimensions, OR within one. Exclusions: any match
  in any dimension = excluded.
- `ApplyModifiers(baseBid, mods Modifiers, ctx ModifierContext) (bid, multiplier)`
  (`modifiers.go`) — percentage modifiers stacked multiplicatively (device × geo ×
  time × day × audience × inventory), clamped per-modifier and combined.
- `NewClassifier()` / `Classifier.Classify(domain, pageURL, declaredCats, keywords)`
  (`contextual.go`) — IAB categories from publisher-declared > URL regex > domain >
  keyword signals; output normalised via `taxonomy.WithParents` (tier-2 always
  carries its tier-1 parent).
- `ComputeQualityScore` / `QualityScore.BidAdjustment` (`quality.go`) and
  `MatchKeywords` (`contextual.go`) exist but currently have no service callers.

## Invariants & Gotchas

- **Hot-path safe, keep it that way.** Every function here is pure — no network
  I/O, no store access. The DSP calls `Evaluate`/`ApplyModifiers` inside the
  per-campaign bid loop, where the iron rule bans per-call I/O (loop data comes
  from in-process copies warmed by `cmd/dsp/refresh.go`). Never add I/O here.
- **Empty = match-all.** An empty inclusion list for a dimension means no
  restriction; empty `Channels` = all channels.
- **`Custom` is dead.** `TargetingSet.Custom` exists on the struct but `Evaluate`
  never checks it (neither include nor exclude).
- **Asymmetric dimensions.** Exclusions skip Languages, InventoryType, Channels —
  those gate via inclusion only.
- **Inclusion failures carry no dimension.** `Result.FailedDimension` is only set
  on exclusion; inclusion misses report just `FailedReason: "no_inclusion_match"`.
- **Geo hierarchy is bidirectional `_`-prefix.** Include "UK" matches request
  "UK_london" AND include "UK_london" matches request "UK" (`containsOrPrefix`).
- **Modifier semantics:** values are percentages (+20 → ×1.20). Zero-valued bounds
  default to +200 / −80 per-modifier and +300 combined. Audience takes the single
  highest matching segment modifier; TimeOfDay takes the first matching window
  (windows may wrap midnight).
- **Consent is the caller's job.** The engine is consent-unaware — the DSP only
  populates `Request.Segments` when the consent verdict allows personalisation.
- **`NewClassifier` compiles ~16 regexes per call.** Construct once and reuse; the
  DSP currently builds one per request only when `Site.Cat` is empty.

## Used By

- `cmd/dsp` — the only service caller (bid handler: classify → `Evaluate` → pacing
  → `ApplyModifiers` → shading). Two caller-side quirks: slate (multi-winner)
  channels nil out category include/exclude before `Evaluate` (category is a soft
  ranking signal there, not a filter), and its `ModifierContext` only sets
  Device/GeoCountry/HourOfDay/Segments — GeoRegion, DayOfWeek and Inventory
  modifiers never fire; HourOfDay is in the campaign's timezone.
- `pkg/models` — embeds `targeting.Rules` / `targeting.Modifiers` in `models.Campaign`.
- `pkg/store/postgres/campaigns.go` — parses `targeting_rules` rows into these
  structs; `parseModifiers` silently drops unknown JSON keys.

## Testing

Pure unit tests in-package (`targeting_test.go`, `contextual_test.go`), no build
tags, no fakes needed.

## Architecture Details

See `docs/PLAN.md` -> "Targeting Exclusions", "Bid Modifiers", "Contextual
Targeting Classification Engine", "Inventory Quality Scoring".
