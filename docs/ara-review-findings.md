# ARA (Attribution Phase 4) — Security Review Findings & Fixes

Adversarial review of the Privacy Sandbox ARA overlay (built 2026-08-12, "Framing
A": real registration headers + report ingest; browser match/noise/delay/
aggregation are the declared mock boundary). Review prompt: `docs/ara-review-prompt.md`.
Design intent: `docs/attribution-phase4-ara.md`.

**Headline:** the money-separation invariant holds — ARA is a physically separate
store, never imported by or joined into billing / reporting / dsp / the exact
conversions stream, and the conversion NATS event carries no ARA data. The one
real hole was a cross-tenant *write* on the aggregatable ingest path. All findings
below are fixed on `main` unless marked otherwise.

## Coverage

| Invariant | Verdict |
|---|---|
| 1. Money-safety / separation | VERIFIED (no change needed) |
| 2. Unauthenticated-write / spoofing | GAP → **fixed** (F1); warn-mode gap surfaced (F2) |
| 3. Tenant isolation (RLS) | VERIFIED |
| 4. Consent gating | VERIFIED |
| 5. Wire-format correctness | VERIFIED; eTLD+1 re-validation added (F5) |
| 6. Default-off + config | VERIFIED |
| 7. Input safety / DoS / idempotency | VERIFIED; purge unbounded → **fixed** (F3) |
| 8. Failure modes | VERIFIED |
| 9. E2E honesty | VERIFIED; extended with a quarantine assertion |
| 10. Dead / half-wired paths | trigger header inert-by-design, now labelled (F6) |

## Findings

### F1 — MEDIUM — Aggregatable report poisoning (cross-tenant write) — FIXED

An aggregatable ARA report structurally carries **no** `source_event_id`. The
original ingest resolved it to an advertiser account via the report's
`attribution_destination` (the newest unexpired source for that destination). But
the destination is the advertiser's **public** site (eTLD+1), nameable by anyone —
so an unauthenticated `POST /.well-known/attribution-reporting/report-aggregate-attribution`
with `{"attribution_destination":"<victim's site>", ...}` resolved to the victim's
account and persisted the attacker's body into the victim's `ara_reports`. The
"a registered source must exist" bound did **not** require the attacker to own that
source, so it did not isolate the victim. Contained to MEDIUM (reporting-only, no
billing, payloads never decrypted), but a genuine tenant-isolation break.

*Fix (chosen approach: quarantine).* Aggregatable reports no longer resolve to a
tenant at all. `ResolveAccount` now resolves **only** by the unguessable
`source_event_id` (the destination fallback is deleted). Aggregatable reports are
parsed and dropped into a new **platform-global** holding table
`ara_aggregatable_quarantine` (migration 096) — no `account_id`, no RLS, never
shown in any advertiser overlay, age-purged (7d), inspectable via SQL only. We can't
verify/decrypt the payloads without the aggregation service (the mock boundary)
anyway, so quarantine is the honest resting place. Event reports are unchanged.
- `pkg/ara/postgres/store.go` — `ResolveAccount(sourceEventID)` (fallback removed),
  new `QuarantineAggregatable(...)`.
- `cmd/tracker/ara.go` — aggregatable ingest branch → quarantine; `adtech_ara_reports_quarantined_total` metric.
- `migrations/096_ara_aggregatable_quarantine.sql`.
- e2e `tests/e2e/ara_test.go` §6 — proves an aggregatable report naming the victim's
  real destination lands in quarantine, **not** the victim's `ara_reports`, and is
  dedup-idempotent.

### F2 — LOW — Warn-mode reopens source registration — FIXED (boot warning)

`/v1/t/ara/src` enforces the beacon signature (so `advid` can't be forged) **only**
when `tracker.signature_validation` is on. In warn mode an unsigned beacon is
accepted, so a caller could record an `ara_sources` row for an arbitrary (existing)
account. This is consistent with how the impression/conversion handlers treat warn
mode, and warn mode is a dev posture — but the cross-tenant-source-write protection
depends entirely on strict mode being on in prod.

*Fix.* `cmd/tracker/main.go` logs a loud `WARN` at boot when `ara_enabled &&
!signature_validation`, so this can't be the silent production posture. (No code
path change — strict mode remains the enforcement.)

### F3 — LOW — Unbounded expired-source purge — FIXED

`DeleteExpiredSources` ran a single `DELETE ... WHERE expires_at < now()` with no
`LIMIT`; a backlog could turn it into one long table-locking transaction.

*Fix.* `pkg/ara/postgres/store.go` — both `DeleteExpiredSources` and the new
`DeleteOldQuarantine` delete in bounded `LIMIT`-batched loops until drained.

### F4 — LOW — Rate-limit private-IP allowlist can bypass ingest — DEPLOYMENT NOTE

The tracker rate limiter allowlists loopback/RFC1918 and skips the limiter for
those IPs. The ingest endpoints are covered **only** while the ingress overwrites
`X-Forwarded-For` with the real public client IP (`TrustedHops=0`). If the ingest
path ever resolves to a private IP (mis-set `TrustedHops`, direct in-cluster reach,
an appending proxy), the limiter is bypassed and only the 128 KiB body cap remains.

*Status.* No code change — this is an ops/ingress invariant. **Deployment
requirement:** the public ingress terminating the tracker MUST overwrite
`X-Forwarded-For` with the real client IP; do not expose the well-known ARA ingest
paths on a path where the resolved client IP lands in an allowlisted range.

### F5 — LOW — Destination not re-validated to eTLD+1 at registration — FIXED

The tracker trusted the signed `dest=` param verbatim. A hand-crafted (but validly
signed) beacon could record a non-eTLD+1 destination that a real browser would
never send.

*Fix.* New `ara.NormalizeDestination` (scheme + registrable domain, strips
path/query/port) is now the single canonical reduction, called by **both** the ad
server (`pkg/adserving/macros.go`, producer) and the tracker
(`cmd/tracker/ara.go`, re-validation at registration). Unit-tested.

### F6 — INFO — Trigger-registration header is inert on the S2S path — LABELLED

`/v1/t/conv` serves `Attribution-Reporting-Register-Trigger`, but this platform's
conversions are server-to-server postbacks and only a real browser acts on the
header — so nothing reads it today. It is served (correctly built) so a future
browser-side conversion beacon registers the ARA trigger with no tracker change.

*Fix.* `cmd/tracker/ara.go` — `setTriggerHeader` is explicitly documented "INERT BY
DESIGN"; the doc's TRIGGER NUANCE already covers the follow-up beacon.

## Author-flagged items (confirmed)

- **Toy `aggregation_keys` ("0x1")** — the aggregatable histogram is a fixed-bucket
  placeholder (never decrypted). Consistent with the mock boundary; with F1 the
  aggregatable path no longer persists anything tenant-facing, so the placeholder
  keys are moot for the overlay.
- **No isolated `pkg/ara/postgres` unit tests (e2e-only)** — acceptable per the
  repo's "never mock Postgres; integration via testcontainers" convention; the
  RLS/hatch/quarantine behaviour needs a real PG. Pure functions
  (`NormalizeDestination`, `ParseAggregatableReport` shared_info recovery) are now
  unit-tested in `pkg/ara`.

## Is it safe to enable in production now (flags on)?

Yes for money-safety (unchanged: ARA can't touch billing or the exact stream).
With F1 fixed, the aggregatable cross-tenant write is closed (reports quarantined,
never tenant-attributed), and F2's boot warning guards the one remaining
strict-mode dependency. Operational prerequisite: F4 — the ingress must set the
real client IP so the ingest rate limit isn't bypassed.
