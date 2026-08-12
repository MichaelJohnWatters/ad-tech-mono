# Attribution Phase 4 — Privacy Sandbox (ARA) — BUILT (Framing A)

**Status:** BUILT 2026-08-12 as **Framing A** (real headers + report ingest;
mock boundary documented). Was parked 2026-08-02. The scoping/decision content
below is retained as the *why*; the box immediately under is *what shipped*.

Supersedes the short stub in `docs/attribution-plan.md` → "Phase 4".

## What shipped (Framing A)

Real, standards-correct, inspectable — a **reporting-only overlay** that never
bills and is stored separately from the exact conversions stream:

- **`pkg/ara`** — `Source`/`Trigger` header builders (ARA wire format: 64-bit
  numerics as JSON strings, expiry clamped to [1d,30d], aggregatable values as
  numbers) + `ParseEventReport`/`ParseAggregatableReport` (aggregatable payloads
  kept encrypted — never decrypted). Unit-tested.
- **migration 095** — `ara_sources` (registration log for account resolution) +
  `ara_reports` (raw browser-posted reports); both RLS + platform hatch. Separate
  tables, **never joined into `conversions`**.
- **tracker** (the single ARA reporting origin) — `GET /v1/t/ara/src` records a
  source + returns `Attribution-Reporting-Register-Source`; `/v1/t/conv` also
  returns `Attribution-Reporting-Register-Trigger`; the well-known
  `report-{event,aggregate}-attribution` endpoints resolve the owning advertiser
  and persist. Registration is consent-gated + `tracker.ara_enabled` (default off),
  and **the source beacon is HMAC-SIGNED** like every other tracker beacon (advid
  is signature-bound; unsigned → 403 in strict mode) so it can't be used to write
  `ara_sources` for an arbitrary account. Report resolution is **non-forgeable**:
  EVENT reports resolve only by the unguessable source_event_id (no destination
  guessing). AGGREGATABLE reports carry no source id and are **never attributed to
  a tenant** — they land in a platform-global quarantine
  (`ara_aggregatable_quarantine`, migration 096), staff-inspectable, never in an
  advertiser overlay. (This closed a cross-tenant write: the destination is public,
  so the earlier destination-fallback let a forged aggregatable POST land in a
  victim's rows — see **Security review** below + `docs/ara-review-findings.md`.)
- **gateway** — `GET /v1/api/ara/reports` (reports:read, account-scoped): the
  advertiser's overlay + summary, labelled "never billed".
- **Path-A completion (2026-08-12):** the ad server bakes the signed
  `attributionsrc` source beacon into served creatives (`adserver.ara_source_registration`,
  consent-gated) + adtech.js registers it; advertiser portal → Attribution tab
  shows the ARA overlay (counts + recent reports, labelled noised/never-billed);
  Prometheus `adtech_ara_*` counters; a 6h expired-source purge; architecture
  diagram updated. Full source→tracker loop e2e: `TestARAAdServerBakesSourceBeacon`.
- **Trigger beacon (2026-08-12, follow-up shipped):** the browser-side ARA trigger
  is now wired — `GET /v1/t/ara/trigger` returns the register-trigger header
  (consent- + `tracker.ara_enabled`-gated, unsigned, never bills), and the
  advertiser's conversion-event setup hands back a ready-to-embed `attributionsrc`
  beacon (`ara_beacon`, portal "Copy ARA" button) to drop on the confirmation page
  alongside the billing pixel. `adtech_ara_triggers_served_total` counter. e2e
  `TestARATriggerBeacon`. (The S2S `/v1/t/conv` still emits the header too, inertly.)
- **e2e** `TestARARegistrationAndReportIngest` — register-source header well-formed
  + recorded → signed conversion returns register-trigger → event report POST
  resolves the account + persists (idempotent retry, orphan dropped) → advertiser
  reads it back through the API. Green against the live stack.

### Security review (2026-08-12)

Adversarial review against the 10 invariants (prompt: `docs/ara-review-prompt.md`;
findings + fixes: `docs/ara-review-findings.md`). Money-separation verified (ARA is
never read/joined by billing/reporting/dsp/conversions). One real hole fixed — the
aggregatable **cross-tenant write** (F1), now closed by quarantining aggregatable
reports out of tenant attribution. Also: boot warning if ARA is on with signatures
off (F2), batched housekeeping deletes (F3), server-side eTLD+1 re-validation of
`dest` (F5), trigger header labelled inert-by-design (F6). Deployment note (F4):
the ingress must set the real client IP so the ingest rate limit isn't bypassed.

### Runbook — verifying with a real browser (the part the Go harness can't)

The match/noise/delay only happen in a real Privacy-Sandbox browser:

1. Chrome with Attribution Reporting enabled (chrome://flags →
   `#privacy-sandbox-ads-apis`, or `--enable-features=AttributionReportingCrossAppWeb`).
2. Set `tracker.ara_enabled=true` and `adserver.ara_source_registration=true`. On a
   CONSENTED serve the ad server bakes a signed `ara_source_url`, and adtech.js
   registers it via `attributionsrc` on the impression pixel — the browser fetches
   it and reads the register-source header. Then, on the advertiser's conversion
   page, embed the ARA trigger beacon (`ara_beacon` from the conversion-event
   setup, or `<img src=".../v1/t/ara/trigger?type=purchase&rev=42" attributionsrc>`)
   — the browser fetches it attribution-eligible and reads the register-trigger
   header. TRIGGER NUANCE: this platform's *billing* conversions are signed
   server-to-server postbacks (`/v1/t/conv`), which a browser never processes; the
   dedicated `/v1/t/ara/trigger` beacon is the browser-side path and is unsigned +
   reporting-only (it never bills), so both can fire on the same confirmation page.
3. Open `chrome://attribution-internals` — the Sources and Triggers tabs show the
   registered entries; after the browser's delay it POSTs event/aggregatable
   reports to the tracker's well-known endpoints, which persist into `ara_reports`.

**Mock boundary (unchanged):** the source↔trigger match, k-anonymity noise,
multi-day delay, and aggregation-service decrypt are the browser's / coordinator's
— not simulated here. ARA is **not** wired into `make test-e2e` beyond the
ingest-endpoint check above.

---

## What ARA is (one paragraph)

Everything we built attributes a conversion by **joining a user across sites** —
advertiser visitor id ↔ hashed email ↔ publisher user id, resolved server-side
through our identity graph (Phase 2). That join is exactly what browsers are
removing: third-party-cookie deprecation, Safari ITP, Firefox. **Privacy Sandbox
Attribution Reporting API (ARA)** moves the join *into the browser*, where it's
still allowed but privacy-constrained: the impression registers an **attribution
source**, the conversion registers a **trigger**, and the **browser** — not us —
matches them and later sends us **noised, aggregated, delayed** reports. The
mobile analogs are Apple's SKAdNetwork / AdAttributionKit. It's a second,
low-resolution attribution *backend* alongside our deterministic/identity one —
not a replacement.

## The decision: defer. Two reasons.

**1. It's the one feature that structurally can't honor this project's contract.**
The project's defining property is **real + end-to-end-proven + no mocks** (trace
browser→ClickHouse, zero slippage, every gap e2e-green — see the mock audit and
the "full local stack is the goal" doctrine). ARA's whole point is that *the
browser does the attribution join*, with noise + aggregation + multi-day delay by
design. To exercise it for real you need real Chrome instances with Privacy
Sandbox enabled, at volume, over days — you **cannot** drive it from the Go
simulator or the e2e harness. So a full build is: real registration headers +
**mocked** browser-match + **mocked** aggregation-report delivery/decryption. The
core would be a mock. That's exactly the "async/cron shell overstated as complete"
shape the mock audit exists to prevent.

**2. Its output collides with the exact-money invariant.** ARA reports are
deliberately noised, aggregated, and delayed — *not* per-conversion. That fights
"settle exact CPA / zero data slippage." Reconciling approximate attribution
against a money ledger is a genuine open problem (see "the reconciliation
question" below); wiring it in naively would muddy the clean money-loop
invariants ([[project_money_loop]], [[project_money_precision]]) we keep exact.

## The reconciliation question (the real research, not the plumbing)

ARA never tells you "conversion X → impression Y for $Z." It tells you, after
delay, either:
- **event-level**: a low-entropy source id + a coarse trigger value, with a
  fraction of reports randomised (k-anonymity noise), or
- **aggregatable**: encrypted histograms only readable via an **aggregation
  service** that adds calibrated Laplace/Gaussian noise per query.

So the design question that gates any build: **what does ARA-attributed spend
*mean* next to deterministic spend?** Options to spec before touching code:
- Treat ARA as a **reporting-only overlay** (never bills) — safest, mirrors how
  Phase 3 multi-touch is reporting-only while last-touch bills. Recommended default.
- Treat ARA as a **fallback estimator** for the cookieless slice, billed against a
  reconciled/modelled number with a documented error bar — needs a statistical
  model and a "don't double-count vs the deterministic stream" story.

Until that's decided, do **not** point ARA at the billing ledger.

## If/when we build it — two framings (pick one up front)

**A. "Real headers, mock boundary documented" (recommended if we do anything).**
Build only the parts that are real and inspectable, and label the rest a mock
boundary in code + docs. Provable via a real browser's
`chrome://attribution-internals`, not via the e2e harness.

**B. "Full local simulation of the browser + aggregation side."** Fake the match,
fake the aggregation service, fake the noise. More surface, but the core is
simulated — only worth it as a teaching artifact, and it must be loudly labelled
as such so it never reads as "ARA works end-to-end here."

Default to **A**. Do not let A silently grow into B.

## Concrete build breakdown (for framing A)

Real, standards-correct, inspectable:
- **Source registration (ad serve).** Adserver returns
  `Attribution-Reporting-Register-Source` on the ad response: `destination` =
  advertiser eTLD+1, `source_event_id`, `aggregation_keys`, `expiry`,
  `filter_data`. Lives next to the existing beacon/macro building in
  `pkg/adserving` + `cmd/adserver`. Gate on consent + a config flag
  (`adserver.ara_source_registration`, default off).
- **Trigger registration (conversion).** The conversion response returns
  `Attribution-Reporting-Register-Trigger` (`event_trigger_data`,
  `aggregatable_trigger_data`, `aggregatable_values`). Natural home: the tracker
  `/v1/t/conv` handler (which we just made per-advertiser-key-aware) and/or the
  advertiser-facing pixel builder in `cmd/gateway/conversions.go`. Config flag
  `tracker.ara_trigger_registration`, default off.
- **Report ingest endpoints.** New tracker/reporting routes for the two report
  callbacks the browser POSTs to (well-known paths under the reporting origin):
  event-level (`.well-known/attribution-reporting/report-event-attribution`) and
  aggregatable (`.../report-aggregate-attribution`). Persist raw reports to a new
  low-resolution stream — **separate table, never joined into the exact
  conversions table** — so the slippage guarantee on the real stream is untouched.

Explicit **mock boundary** (label in code + this doc, don't pretend it's real):
- the **browser match** (only a real Sandbox browser does it),
- the **aggregation service** decrypt+noise step (Google/coordinator infra;
  stub the interface, document the shape),
- any **billing** off ARA numbers (blocked on the reconciliation decision above).

## Verification story (honest)

- **What CAN be proven locally:** the headers are well-formed and standards-correct
  (unit tests on the header builders); a real Chrome with Privacy Sandbox flags on
  registers the source/trigger and shows them in `chrome://attribution-internals`
  (manual runbook, not automated e2e); the report-ingest endpoints accept and
  persist a captured/sample report body.
- **What CANNOT be proven by the Go e2e harness:** the actual match, the noise, the
  delay, the aggregation. Say so; don't wire it into `make test-e2e`.

## Cross-cutting when built

- New NATS subject if ARA reports fan out (`adtech.events.ara_report`?) → add to
  `pkg/events/subjects.go` + PLAN.md "NATS Subjects".
- Multi-tenancy: report ingest scoped to the advertiser account like every other
  store method.
- Privacy: source/trigger registration stays consent-gated
  (`privacy.Evaluate().Personalise`) — organic/unconsented → no registration.
- Diagram: a new attribution stream + report-ingest path → update the matching
  `docs/diagrams/*.d2` per `docs/diagrams/README.md`.
- New surface ops: if a report-ingest consumer is added, it follows the
  deaf-on-boot / single-replica doctrine like the other consumers.

## Effort / risk

**Effort:** L, mostly research/spec (the reconciliation model, not the plumbing).
**Risk:** high — external platform surface that's still shifting, and the one place
the "everything is real + proven" contract can't fully hold. **Deps:** builds on
Phases 0–2; plannable independently. **Prereq to start:** decide the reconciliation
framing (reporting-only overlay vs billed estimator) and the A-vs-B build framing
*first* — those decisions shape everything else.
