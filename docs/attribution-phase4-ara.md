# Attribution Phase 4 — Privacy Sandbox (ARA) — DEFERRED, scoping doc

**Status:** parked on purpose (2026-08-02). Not started, no code. This doc is the
"pick it up later" reference — the decision, *why*, and a concrete build
breakdown with the honest boundary marked. Phases 0–3 + gaps G1–G7 + the identity
hardening are all shipped and e2e-proven; ARA is the point of diminishing returns
where the effort stops being demonstrable the way everything else is.

Supersedes the short stub in `docs/attribution-plan.md` → "Phase 4".

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
