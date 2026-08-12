# Review: Privacy Sandbox ARA (attribution Phase 4)

Self-contained, omission-focused review prompt for the Privacy Sandbox ARA work
(built as "Framing A" — see [`attribution-phase4-ara.md`](attribution-phase4-ara.md)).
Hand it to a fresh context. It's calibrated to make the reviewer re-derive from the
code and try to break the invariants, not rubber-stamp the summary. Companion to
[`SECURITY-AUDIT-PROMPT.md`](SECURITY-AUDIT-PROMPT.md).

---

You are a senior reviewer auditing a just-shipped feature in a Go monorepo at
/Users/michaeljohnwatters/repo/ad-tech-mono. The feature is Privacy Sandbox
Attribution Reporting API (ARA) support, built as "Framing A" (real registration
headers + report ingest; the browser match/noise/delay/aggregation are a declared
mock boundary). The author claims it is a REPORTING-ONLY overlay: never bills,
stored in a SEPARATE table, never joined into the exact conversions stream, tenant-
isolated, consent-gated, signature-enforced, default-off.

Your job is to VERIFY those claims against the code and to HUNT for what's wrong or
missing — do not trust the summary or the commit messages. Read the actual code,
cite every finding as file:line, and be adversarial (assume a bug/hole exists).
Do not modify files. The design intent is in docs/attribution-phase4-ara.md; treat
it as intent, not proof.

## Artifacts to read
- pkg/ara/ara.go, store.go, ara_test.go; pkg/ara/postgres/store.go
- migrations/095_ara_reports.sql
- cmd/tracker/ara.go + its wiring in cmd/tracker/main.go
- pkg/adserving/macros.go (BuildARASourceURL, araDestination) + pkg/adserving/ara_test.go
- cmd/adserver/main.go (serve assembly) + pkg/models/models.go (ServeResponse.ARASourceURL)
- cmd/gateway/ara.go (+ route/registration in cmd/gateway/main.go)
- web/static/adtech.js (attributionsrc), web/templates/portal/advertiser.html (loadARA)
- config keys: tracker.ara_enabled, adserver.ara_source_registration (pkg/config/keys)
- e2e: tests/e2e/ara_test.go, tests/e2e/ara_adserver_test.go

## Invariants to prove or break (this is the core of the review)

1. MONEY-SAFETY / SEPARATION. Prove ara_reports/ara_sources are NEVER read or
   joined by any billing, settlement, or exact-conversions path. Grep the whole
   repo for `ara_` and for imports of pkg/ara outside the ARA files; trace each hit.
   Confirm nothing in pkg/billing, cmd/reporting, or the conversions path touches
   ARA. A single join/read into the exact stream is a HIGH finding.

2. UNAUTHENTICATED-WRITE / SPOOFING. The report-ingest endpoints are unauthenticated
   (browsers POST). Source registration (/v1/t/ara/src) takes advid as a param.
   - Confirm /v1/t/ara/src actually enforces a signature (ValidateSignatureAny +
     sigKeysForAdvertiser) so advid can't be forged, AND what happens when
     tracker.signature_validation is FALSE (warn mode) — is the write reopened?
     Is that acceptable/consistent? Cite the exact gate.
   - Confirm EVENT reports resolve ONLY by the unguessable source_event_id and do
     NOT fall back to the (public) attribution_destination. Then reason about the
     AGGREGATABLE destination fallback: can an attacker write rows into another
     advertiser's account? What bounds it? Is that bound real?
   - Try to construct a poisoning path end-to-end and state whether it works.

3. TENANT ISOLATION. Both tables must have RLS + the platform-read hatch with the
   NULLIF empty-safe shape (mig 087 style). Verify RecordSource/SaveReport are
   tenant-scoped (GUC) and ResolveAccount/DeleteExpiredSources use the hatch
   deliberately. Confirm the read API (/v1/api/ara/reports) is account-scoped and
   can't be widened by a header/param. Confirm the app connects as the NOBYPASSRLS
   role so RLS actually bites.

4. CONSENT. Source + trigger registration and the adserver beacon must be gated on
   privacy.Evaluate(...).Personalise (or the equivalent consent signal). Verify an
   opted-out / GPC / no-consent request registers NOTHING and bakes no beacon.

5. STANDARDS-CORRECTNESS of the wire format. Check the header builders against the
   ARA spec: 64-bit numerics as JSON STRINGS, aggregatable_values as NUMBERS,
   source expiry clamped to [1d,30d], destination = scheme + registrable domain
   (eTLD+1). Check report parsers keep aggregatable payloads ENCRYPTED (never
   decrypted). Flag anything a real browser would reject.

6. DEFAULT-OFF + CONFIG. Confirm both flags default false and are the only way ARA
   activates; nothing runs when off.

7. INPUT SAFETY / DoS / IDEMPOTENCY. Ingest body-size cap? Rate-limit coverage on
   the well-known paths (note the allowlist bypasses private IPs)? UUID/length
   validation on the source beacon? Per-account report_id dedup (retry-safe)?
   Expired-source purge actually bounded? Any unbounded growth or 500-on-bad-input?

8. FAILURE MODES. DB unavailable (store nil), malformed report bodies, unresolved
   reports — verify they degrade sanely (e.g. accepted-but-dropped 200 so browsers
   don't retry forever) and can't crash or leak.

9. E2E HONESTY. Do tests/e2e/ara_test.go + ara_adserver_test.go actually exercise
   the REAL parts (registration, resolution, persistence, dedup, drop, adserver→
   tracker loop), and do they AVOID pretending to test the browser match/noise/
   delay? Are the assertions meaningful or would they pass trivially? Is anything
   asserted that can't actually be true locally?

10. DEAD/HALF-WIRED PATHS. The /v1/t/conv register-trigger header is served, but
    the author says conversions are S2S so no browser trigger beacon exists. Is the
    trigger header therefore effectively dead? Is anything else built-but-unreachable
    (e.g. adtech.js attributionsrc with no ara_source_url ever set)? Is the whole
    feature actually reachable through the running stack, or only via the e2e?

## Also independently confirm these author-flagged items (verify, don't take on faith)
- Signature enforcement is conditional on strict mode; is that a real hole in warn?
- Aggregatable poisoning is "bounded, not closed" — is the bound adequate?
- Toy aggregation_keys ("0x1") — do they make the aggregatable path meaningless?
- No isolated tests for pkg/ara/postgres (e2e-only) — acceptable?

## Output
1. Coverage matrix: invariant × (verified / GAP / N-A) with a one-line note each.
2. Findings, each {severity high|med|low, invariant, file:line, what's wrong/missing,
   why it matters, the fix}, sorted by severity.
3. Verdict per invariant (OK / GAP) + a "most important 3 fixes" list.
4. One explicit sentence: is this safe to enable in production as-is (flags on)? If
   not, what's the minimum to make it so?

Rules: read the real code; cite file:line for every claim; do not trust
comments/commit messages/the design doc as evidence of behavior; where a hole is
suspected, trace the request path to confirm rather than pattern-match.
