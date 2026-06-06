# Event Pathway Audit (#9)

> **Status:** Inventory pass complete (2026-06-06). 6 confirmed gaps
> below. Wiring/logging/dashboard checks pending.

Source of truth for every NATS-published event in the platform: who
publishes, who consumes, what lands downstream (analytics + billing
ledger), and where the gaps are. Generated from a sweep of
`pkg/events/subjects.go`, `pkg/events/publisher.go`,
`cmd/**/main.go`, `cmd/reporting/main.go`, and
`pkg/store/analytics/`. Refresh by re-running the greps in the
"How to regenerate" section.

## Domain events (17 declared subjects)

The "interesting" events — auction outcomes, impressions, revenue
flow. Cache invalidate subjects are in their own section.

| # | Subject | Publisher | Consumer | Analytics landing | Billing landing | Status |
|---|---|---|---|---|---|---|
| 1 | `adtech.events.impression` | `cmd/tracker/main.go:166` (`publisher.publishImpression`) | `cmd/reporting:handleImpression` | `analytics.InsertImpression` | `Engine.ProcessEvent` (CPM bills, CPC/CPA/vCPM reserve) | ✅ |
| 2 | `adtech.events.click` | `cmd/tracker/main.go:209` (`publisher.publishClick`) | `cmd/reporting:handleClick` | `analytics.InsertClick` | `Engine.SettleByTrace(…, "click")` (CPC) | ✅ |
| 3 | `adtech.events.conversion` | `cmd/tracker/main.go:245` (`publisher.publishConversion`) | `cmd/reporting:handleConversion` | `analytics.InsertConversion` | `Engine.SettleByTrace(…, "conversion")` (CPA) | ✅ |
| 4 | `adtech.events.view` | `cmd/tracker/main.go:324` (`publisher.publishView`) | `cmd/reporting:handleView` + `handleViewabilityFromTracker` | `analytics.InsertView` | vCPM settle when `IABViewable=true` | ✅ |
| 5 | `adtech.events.video` | `cmd/tracker/main.go:357` (`publisher.publishVideo`) | `cmd/reporting:handleVideo` | TBD — verify in handler | (none — analytics-only) | ✅ pub+consume; verify analytics |
| 6 | `adtech.events.audio` | `cmd/tracker/main.go:369` (`publisher.publishAudio`) | `cmd/reporting:handleAudio` | TBD — verify in handler | (none — analytics-only) | ✅ pub+consume; verify analytics |
| 7 | `adtech.auction.win` | `cmd/exchange/main.go:556` (`pub.AuctionWin`) | `cmd/reporting:handleAuctionWin` | `analytics.InsertAuctionWin` | (none — `AuctionWinEvent` shape is the trigger for `handleImpression`'s billing path) | ✅ (DuckDB schema + INSERT shipped 2026-06-06, Gap D) |
| 8 | `adtech.auction.complete` | `cmd/exchange/main.go:581` (`pub.AuctionComplete`) | `cmd/reporting:handleAuction` | `analytics.InsertAuctionWin` | — | ✅ |
| 9 | `adtech.direct.win` | `cmd/publisher-adserver/main.go:306` (`d.pub.DirectWin`) | `cmd/reporting:handleDirectWin` | `analytics.InsertAuctionWin` | (CPM only — direct CPC/CPA is backlog #4) | ✅ |
| 10 | `adtech.prebid.outbound.win` | `cmd/publisher-adserver/main.go:488` (`d.pub.PrebidOutboundWin`) | `cmd/reporting:handlePrebidOutboundWin` | `analytics.InsertAuctionWin` | — | ✅ |
| 11 | `adtech.serve.nofill` | `cmd/publisher-adserver/main.go:469` (`d.pub.ServeNoFill`) | `cmd/reporting:handleServeNoFill` | TBD — verify in handler | — | ✅ pub+consume; verify analytics |
| 12 | `adtech.budget.depleted` | `cmd/dsp/main.go:642` (`pub.BudgetDepleted`) | `cmd/reporting:handleBudgetDepleted` | `analytics.InsertBudgetDepletion` | — | ✅ |
| 13 | `adtech.campaign.state_changed` | `cmd/dsp/management.go:publishCampaignStateChange` (handlePatch + handleDelete) | `cmd/reporting:handleCampaignState` | `analytics.InsertCampaignStateChange` | — | ✅ shipped 2026-06-06 (Gap A) |
| 14 | `adtech.privacy.opt_out` | **declared, never published** | **no consumer** | — | — | ❌ Gap B (backlog #1) |
| 15 | `adtech.privacy.deletion_requested` | **declared, never published** | **no consumer** | — | — | ❌ Gap B (backlog #1) |
| 16 | `adtech.privacy.deletion_completed` | **declared, never published** | **no consumer** | — | — | ❌ Gap B (backlog #1) |
| 17 | `adtech.webhooks` | **declared, never published** | **no consumer** | — | — | ❌ Gap C (the `cmd/webhooks` service is an empty shell) |
| 18 | `adtech.tracker.rejected` | `cmd/tracker/main.go:publishRejected` (HMAC strict / fraud / dedup sites) | `cmd/reporting:handleTrackerRejected` | `analytics.InsertTrackerRejection` | — | ✅ shipped 2026-06-06 (Gap E) |
| 19 | `adtech.adserver.render_failed` | `cmd/adserver/main.go:serveHandler` (creative resolver miss) | `cmd/reporting:handleRenderFailed` | `analytics.InsertRenderFailure` | — | ✅ shipped 2026-06-06 (Gap F) |

## Cache invalidate subjects (14 declared)

These are fire-and-forget invalidations consumed by warm caches
(`pkg/cache/warm`). Each cache subscribes via `InvalidateSubject:` on
its `warm.Cache` config. Publisher is whichever service mutates the
underlying data (CRUD handlers + the gateway reset endpoint).

| Subject | Published by | Consumed by |
|---|---|---|
| `adtech.cache.invalidate.campaigns` | `cmd/dsp/management.go:481`, `cmd/gateway/reset.go:166` | DSP campaign cache (`cmd/dsp/main.go:392`) |
| `adtech.cache.invalidate.placements` | `cmd/ssp/management.go:354`, `cmd/gateway/reset.go:167` | SSP placement cache (`cmd/ssp/main.go:172`), pubad placement cache (`cmd/publisher-adserver/main.go:648`) |
| `adtech.cache.invalidate.creatives` | `cmd/gateway/reset.go:168` | adserver creative cache (`cmd/adserver/main.go:182`) |
| `adtech.cache.invalidate.publishers` | `cmd/gateway/reset.go:169` | (consumer TBD — verify per-service warm cache) |
| `adtech.cache.invalidate.deals` | `cmd/gateway/reset.go:170` | exchange deal cache (`cmd/exchange/main.go:227`) |
| `adtech.cache.invalidate.secrets` | `cmd/gateway/secrets.go:306` | every service's secrets cache (per service `RefreshCache` endpoint) |
| `adtech.cache.invalidate.config` | gateway `PUT /v1/config` (built into `pkg/config/manager.Set*`) | every service's config Manager (subscription in `pkg/config/setup.go`) |
| `adtech.cache.invalidate.publisher-line-items` | (only the reset path) | pubad line-item cache (`cmd/publisher-adserver/main.go:626`) |
| `adtech.cache.invalidate.billing-rates` | (only the reset path) | reporting billing-rate cache (`cmd/reporting/main.go:716`) |
| `adtech.cache.invalidate.dsp-endpoints` | **declared, no publish + no subscribe found** | — |
| `adtech.cache.invalidate.fraud-rules` | **declared, no publish + no subscribe found** | — |
| `adtech.cache.invalidate.opt-outs` | **declared, no publish + no subscribe found** | — |
| `adtech.cache.invalidate.webhook-subs` | **declared, no publish + no subscribe found** | — |
| `adtech.cache.invalidate.signing-keys` | **declared, no publish + no subscribe found** | — |

Five cache-invalidate subjects are defined but unwired on both
sides. They're forward-declarations for backlog work (fraud rules,
opt-outs, webhooks, signing keys, DSP endpoints) — not bugs.

## Confirmed gaps

**Gap A — Campaign state changes never flow.** ✅ shipped 2026-06-06
- `cmd/dsp/management.go` (`handlePatch`, `handleDelete`) reads the
  pre-update status and publishes `CampaignStateEvent` only on actual
  transitions (no-op patches don't generate bus noise).
- `cmd/reporting.handleCampaignState` consumes and records via
  `analytics.InsertCampaignStateChange`.
- Verified by `TestCampaignStateChangeReachesReporting` (e2e).
- Bonus: a `DebugCampaignStateChanges` endpoint lets ops dashboards
  show per-campaign pause/resume timelines.

**Gap B — Privacy events are vapor.**
Three subjects (`SubjectPrivacyOptOut`, `…DeletionRequested`,
`…DeletionCompleted`) + `Publisher.OptOut` + the event payloads all
exist, but **zero call sites**. Same as backlog #1. Currently four
`TestPrivacy*` e2e tests skip with "consent/opt-out not wired into
ad server / DSP bid path yet". Lands as part of backlog #1.

**Gap C — Webhooks subject has no service to publish or consume.**
`SubjectWebhook = "adtech.webhooks"` exists but `cmd/webhooks/` is
an empty shell. Either: (a) build the webhooks service that
subscribes to interesting subjects (auction.win, conversion,
budget.depleted) and POSTs to registered URLs, then publish
`adtech.webhooks` only as a delivery-trace stream; (b) demote
`SubjectWebhook` since its purpose is unclear.

**Gap D — `analytics.InsertAuctionWin` is a no-op in DuckDB.** ✅ shipped 2026-06-06
- Added `auction_wins` table to the DuckDB schema (15 columns mirroring
  the `AuctionWinEvent` wire shape).
- Wired `InsertAuctionWin` to actually `INSERT` instead of returning
  `nil`. All three handlers (`handleAuctionWin`, `handleDirectWin`,
  `handlePrebidOutboundWin`) now land in DuckDB.
- Guard test `TestDuckDB_InsertAuctionWin` (build-tagged `duckdb`)
  proves the schema + INSERT round-trip.

**Gap E — Tracker rejection events not published.** ✅ shipped 2026-06-06
- New subject `adtech.tracker.rejected` + `TrackerRejectedEvent`
  payload (TraceID, EventType, Reason, Detail, Timestamp).
- `cmd/tracker/main.go` `publishRejected` fires from every rejection
  site: HMAC strict-mode 403, fraud check blocked (with the underlying
  reasons in Detail), and dedup hit. Goroutine-published so the pixel
  response stays sub-10ms.
- `cmd/reporting.handleTrackerRejected` records via
  `analytics.InsertTrackerRejection`. Memory-store readers
  (`TrackerRejectionsByTrace` filterable by reason +
  `TrackerRejectionsByReason` for totals).
- Debug endpoint `/debug/tracker_rejections?trace_id=` and `?reason=`.
- Verified by `TestTrackerRejectedEventDedup` (5 fires → 4 dedup
  rejections + 1 first-seen) and `TestTrackerRejectedEventHMACStrict`
  (strict-mode unsigned → rejected with reason=invalid_signature).

**Gap F — Ad server render failures not published.** ✅ shipped 2026-06-06
- New subject `adtech.adserver.render_failed` + `AdserverRenderFailedEvent`
  (TraceID, CampaignID, CreativeID, PlacementID, PublisherID, Reason,
  Detail, Timestamp).
- `cmd/adserver/main.go` serveHandler now publishes the event when
  the creative resolver misses (`reason=unknown_creative`,
  `detail=<requested creative_id>`). Falls back to placeholder HTML
  as before — the browser still sees a 200, but reporting now has
  the side-channel signal.
- `cmd/reporting.handleRenderFailed` records via
  `analytics.InsertRenderFailure` +
  `analytics.RenderFailuresByCreative` reader.
- Debug endpoint `/debug/render_failures?creative_id=`.
- Verified by `TestAdserverRenderFailedEventForUnknownCreative`
  (e2e): ServeAd with an unresolvable creative_id → placeholder
  HTML returned + render_failed event lands with reason=unknown_creative.

## Sub-task status

| Sub-task | Status |
|---|---|
| Inventory pass (who publishes / consumes what) | ✅ shipped — table at top |
| `schema_version` policy | ✅ shipped 2026-06-06 — `CurrentSchemaVersion = 1` in `pkg/events`, reflection test in `payloads_test.go` |
| Logging coverage + event-emission completeness audit | ✅ shipped 2026-06-06 — see below |
| ACK/NAK + idempotency review | Pending |
| Dashboards + observability (per-subject publish/consume/lag) | Pending |

## Logging coverage + event-emission completeness audit (2026-06-06)

Goal: confirm every NATS publish carries a structured slog line with
`trace_id` + a business identifier, every consumer logs receipt with
the same, and surface business-meaningful actions that should be
emitted as events but aren't.

### Reference patterns to preserve

- `cmd/tracker/main.go` impression handler: `reqLog.Info("impression",
  "campaign_id", …, "placement_id", …)` fires **before** the async
  publish goroutine.
- `cmd/dsp/management.go:publishCampaignStateChange`: explicit
  state-change event with pre-publish `log.Info("published campaign
  state change", "campaign_id", …, "old", …, "new", …)`.
- `cmd/exchange/main.go`: `reqLog` context carries `trace_id` through
  fan-out and into the async publish via `context.WithoutCancel(ctx)`.

### Logging-coverage gaps (publish sites)

Small but real — three handlers publish without an accompanying
slog line that carries both `trace_id` and a business identifier:

- **A1 — `cmd/tracker/main.go` view pixel handler** (around the
  `publisher.publishView` call): no pre-publish log line. Fix: add
  `reqLog.Info("view", "campaign_id", …, "duration_ms", …,
  "iab_viewable", …)` before the publish goroutine.
- **A2 — `cmd/tracker/main.go` video / audio handlers**: log the
  `event_type` but no business identifiers (campaign_id /
  placement_id / publisher_id). Fix: include whichever URL params
  the handler already has access to.
- **A3 — `cmd/publisher-adserver/main.go` direct.win / serve.nofill /
  prebid.outbound.win publishes** (lines 306, 469, 488): goroutine
  publishes with no pre-publish slog line in the handler scope. Fix:
  add a `reqLog.Info` per branch before the `go d.pub.X(...)` call.

Consumers in `cmd/reporting/main.go` all log receipt correctly —
no structural gaps on the consume side.

### Event-emission completeness gaps

11 business-meaningful actions either log without an event or have
no observable signal at all. Severity ranks roughly:

- **B1 — Account creation (gateway signup)** — no handler found.
  No log, no event. Audit gap.
- **B2 — Account auto-create on DSP CRUD** —
  `cmd/dsp/management.go:ensureMgmtAdvertiser` silently upserts an
  accounts row. No log, no event. Audit gap.
- **B3 — Login success / failure** — no audit trail. Security gap.
- **B4 — Bootstrap key mint** — `cmd/gateway/bootstrap.go` logs
  the mint but emits no event. Could ride an `adtech.audit.*`
  subject if/when we build one.
- **B5 — Role / permission changes** — no handler found.
- **B6 — Publisher CRUD** — no dedicated CRUD handlers found
  (publishers come through seed only).
- **B7 — Creative create** — silent (part of `writeNewCampaign`,
  no success log).
- **B8 — Creative approve / reject** — no handler found.
- **B9 — Deal CRUD** — no handlers found (deals are seed-only
  today).
- **B10 — Frequency-cap block at adserver** —
  `cmd/adserver/main.go` line ~292 logs "ad blocked by freq cap"
  but doesn't emit. Worth an event: ops can dashboard +
  advertisers can see "we suppressed N over-cap serves." Could be
  a small `adtech.adserver.freq_cap_blocked` subject following
  the same shape as `adtech.tracker.rejected`.
- **B11 — Pod register / heartbeat** — `pkg/config/registry`
  writes to `service_registry` but no audit event. Ops can see
  "pod X joined" only via log scraping. Probably fine for now.

A coherent next pass would frame B1-B6 + B8 + B9 + B11 as an
"audit-log subject" — one new `adtech.audit.*` family that captures
account / auth / role / CRUD lifecycle events. That work pairs
naturally with backlog #7 (real auth on management endpoints) since
auth + audit logging are conventionally built together.

B10 (freq cap event) is the only one that's a no-decision quick fix —
follows the existing `tracker.rejected` template.

### Trace-ID propagation

HTTP-inbound handlers all extract `trace_id` from the request and
propagate via `logger.WithTraceID(ctx, traceID)` → `reqLog :=
logger.WithContext(log, ctx)`. Reference flow verified in tracker
impression, exchange auction, ssp request, pubad serve, adserver
serve. All publish sites use the reqLog context, so the trace_id
chain holds end-to-end on the publish side.

`cmd/reporting/main.go` consumers extract `trace_id` from the
decoded payload and include it as a slog field but don't push it
back into context via `logger.WithTraceID()`. Acceptable for
fire-and-forget handlers (no downstream calls would inherit it
anyway) but worth standardizing if reporting ever grows
synchronous downstream calls.

## How to regenerate

```bash
# Declared subjects
grep -E "^\s*Subject[A-Z]\w*\s*=" pkg/events/subjects.go

# Publishers (typed)
grep -rn "pub\.\(AuctionWin\|AuctionComplete\|BudgetDepleted\|CampaignStateChanged\|OptOut\|DirectWin\|PrebidOutboundWin\|Video\|Audio\|ServeNoFill\)\|publisher\.publish\(Impression\|Click\|Conversion\|View\|Video\|Audio\)" cmd/

# Publishers (raw bus.Publish)
grep -rn "bus\.Publish\|p\.bus\.Publish" cmd/ | grep -v _test

# Consumers
grep -n "events\.Subject" cmd/reporting/main.go

# Analytics inserts
grep -n "Insert\(Impression\|Click\|Conversion\|View\|AuctionWin\|BudgetDepletion\)" \
  cmd/reporting/main.go pkg/store/analytics/*.go | grep -v _test
```

## Next step

Plug the 6 confirmed gaps in priority order. Likely first one: **Gap A
(campaign.state_changed)** — smallest, no scope question, adds 30 min
of work and one e2e test. Then **Gap D (DuckDB AuctionWin insert)** —
silent data loss is the most expensive class of bug to leave in. Then
the larger ones (B/E/F) get folded into backlog items #1/#2/#3 as
those sessions kick off.
