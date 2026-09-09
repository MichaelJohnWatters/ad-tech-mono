# pkg/audience - Audience Segments

Segment definitions + membership storage, and the read path that puts a user's
segments on the bid hot path (SSP stamps `user.ext.segments`; DSP enriches
targeting with its private segments).

## Key Entry Points

- `audience.go` — **in-memory** `Store` (segments, membership, composite eval).
  Unit-test fake + `pkg/privacy` deletion propagation only; loses everything on restart.
- `store/store.go` — `Lookup` interface, the per-bid read contract:
  `SegmentsForUser` (public, SSP side) + `DSPSegmentsForUser` (dsp_private, DSP side).
- `store/postgres/` — the real store over `audience_segments` /
  `audience_segment_members` (mig 019): segment CRUD, `AddMembers` /
  `AddMembersWithExpiry` (TTL'd retargeting), changelog outbox
  append/read/trim, suppression burn-lists (mig 082), product views (mig 084),
  taxonomy + data fees (migs 062/063), marketplace `EstimateOverlap`.
- `store/cached/` — Redis L2 wrapper around a fallback `Lookup` (negative-caches
  empty results). Fallback path only when the preloader fails to start.
- `store/preload/` — the normal DSP/SSP bid-path reader: **READ-ONLY** `SMEMBERS`
  on Redis SETs `audience:set:{user}:{visibility}`. `Start`/`Refresh` are no-ops.

## Invariants & Gotchas (CRITICAL)

- **Single-writer cache.** Membership Redis SETs are maintained ONLY by
  `cmd/pipeline`'s `audience_cache_writer`: a mig-078 trigger appends every
  member insert/delete to `audience_membership_changelog` (same tx), the writer
  drains it (SADD/SREM, `audience.changelog_poll_interval` 3s) and full-scan
  reconciles (`ReplaceSet`, `audience.changelog_reconcile_interval` 5m).
  Reconcile MUST tombstone vanished users (pruned/TTL-expired) via the
  prev-keys diff — else stale targeting. Drain/reconcile are mutex-serialized
  in the writer. Never add a second writer or per-pod rebuild.
  See `docs/AUDIENCE_DATA_PATH_SCALING.md`.
- **Hot-path rule:** `Lookup` impls must never block bidding — Redis blip
  degrades to empty ("no segments") at debug level, not an error per bid.
- **Visibility split:** `public` segments ride outbound bid requests to every
  DSP; `dsp_private` (CRM, retargeting) must NEVER leak into the bid request.
- **RLS:** cross-account reads (bid fan-out, cache reconcile, overlap) go
  through `withPlatformRead` (`app.platform_read='on'`); tenant writes through
  `withTenant` (`app.current_account_id` GUC). A bare-pool query on these
  tables under the app role silently returns 0 rows.
- **Lineage is first-writer-wins:** `AddMembers*` stamp `source`/`origin_trace`
  (mig 080); ON CONFLICT never overwrites the original origin.
- `AddMembersWithExpiry` returns NEWLY-inserted count only (`xmax = 0`) — what
  makes real-time retargeting first-enroll-only; repeat visits silently extend
  the window.
- `RemoveMembersNotIn(nil)` must prune everyone — nil is forced to `[]`
  (`NOT (x = ANY(NULL))` is NULL and deletes nothing).
- **No boot-time ping gate** in the DSP/SSP `openAudienceStore`: `sql.Open` is
  lazy on purpose — a cold-boot ping race once disabled segments for a pod's
  entire life (silent no-bid on every segment-targeted campaign).

## Used By

DSP + SSP (bid-path `Lookup` via preload/cached — `openAudienceStore` in each
service's `main.go`), `cmd/pipeline` (cache writer, onboarding),
`cmd/audience-rt` (real-time enroll/suppress), gateway (portal audiences/
retargeting/taxonomy/marketplace), adserver (dynamic products),
`pkg/profilebuilder`, `pkg/ingest`, `pkg/privacy` (in-memory store).

## Testing

`store/postgres/withplatformread_integration_test.go` (`-tags=integration`,
real Postgres) proves the platform-read hatch; the in-memory root store is the
unit-test fake — no DB mocks.

## Pointers

- `docs/PLAN.md` -> "Audience Segment Delivery (Read Path)", "Unified Audience
  Management Layer", "Audience segments moved off the bid hot path"
- `docs/AUDIENCE-PIPELINE.md` (end-to-end explainer), `docs/AUDIENCE_DATA_PATH_SCALING.md`
  (append-based cache design), `docs/AUDIENCE_DATA_INGEST.md`
