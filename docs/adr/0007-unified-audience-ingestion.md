# ADR 0007 — Unified, durable audience file ingestion (one queue, one processor)

**Status:** Accepted — all 4 phases shipped + verified (2026-07-23). Follow-up
considered but not built: schema-drift detection and synchronous pre-upload schema
rejection — today validation is lenient (accept → stage → per-row quarantine at
process time), not an up-front whole-file schema gate.
**Related:** ADR 0006 (ClickHouse-primary; onboarding now publishes `profile.signal`
to NATS → reporting → ClickHouse), the `targeting-data-flow` / `data-lifecycle`
diagrams, `pkg/reportjobs` + `cmd/report-runner` (the queue+worker pattern this copies).

## Context

Audience file ingestion has **two code paths doing the same work**:

- **Gateway** (`cmd/gateway/audiences.go`) — a 1st-party CSV upload is matched and
  written to `audience_segment_members` **synchronously in the request**, returning a
  match rate. Capped at `maxAudienceUploadRows`; larger files are told to "use the
  partner drop-zone."
- **Pipeline** (`cmd/pipeline/onboarding.go`) — the 3rd-party drop-zone poller does the
  identical decode → validate → normalise → match → `AddMembers` → `onboarding_runs`
  → publish `profile.signal`, **asynchronously, any size**.

> **Note (delivery):** the drop-zone is **INTERNAL-ONLY** today. Its processing is
> real, but it presupposes a file already in the onboarding bucket, and we do not
> support direct bucket access — so external providers can't self-deliver; files
> arrive via platform-managed S3 creds. The customer-facing path is the gateway
> upload (which handles big files via 202 → the same worker). Opening the drop-zone
> externally needs a delivery broker (presigned prefix-scoped PUT URLs, an authed
> streaming upload, or per-provider creds + per-prefix IAM) — deferred.

Problems:
1. **Two paths, one job.** The synchronous path duplicates the async one and imposes an
   awkward size cutoff. Real DMPs/CDPs treat *all* audience ingestion as async jobs with
   status (queued → matching → matched/failed), match-rate reporting, and quarantine —
   synchronous in-request matching is the outlier here.
2. **The sync path isn't durable.** A request that dies mid-match loses the work; there's
   no retry, no lease, no record of an in-flight ingest.
3. **No way to schedule ingest.** "Upload now, don't process until date T" (data
   licensing / embargo / point-in-time freshness) is impossible.

Note on scope: **go-live timing is NOT part of this.** When an audience is *served* is
controlled by line-item **flight dates** (already enforced by the day-boundary job). This
ADR is about when a file is *processed*, which is orthogonal.

## Decision

**One durable job queue, one processor, two thin producers. Small due-now files are
processed inline (instant match rate); everything else by a worker. Scheduling is a
`run_at` column.**

1. **`audience_ingest_jobs`** — a Postgres queue that mirrors `report_jobs`
   (`FOR UPDATE SKIP LOCKED` + lease-based requeue). It **folds `onboarding_runs`**: the
   job row carries the queue state AND the terminal result counts (total/valid/rejected/
   matched rows, match_rate, rejected_key). `onboarding_runs` is retired.
2. **Every ingest stages the file to S3 and inserts one job row** — 1st- or 3rd-party,
   small or large, inline or deferred. There is no bypass and nothing is "pretended": a
   small upload produces a genuine `done` job row identical in shape to a worker-run one.
3. **`processStagedFile(ctx, job) → result`** is the single processor — the existing
   onboarding stages refactored into one reusable function (decode → validate → normalise
   → match → `AddMembers` → publish `profile.signal` → quarantine). Both producers'
   files flow through it.
4. **Two execution modes, chosen by a predicate, over the same processor + record + lease:**
   - `rowCount ≤ ingest.inline_max_rows` **AND** `run_at ≤ now` → the **gateway processes
     it inline** (claims the row with a lease, runs `processStagedFile`, marks `done`),
     returns **200 + match_rate**. Instant feedback preserved.
   - otherwise → row stays `queued`; the **pipeline ingest worker** drains it. Gateway
     returns **202 + job_id**.
   - Held/scheduled files (`run_at` in the future) never go inline — you can't "ingest
     now" something you're holding.
5. **`run_at TIMESTAMPTZ` is the scheduling / `hold_until` lever.** `ClaimOne` filters
   `status='queued' AND run_at ≤ now()`. A held file is **staged but never opened** until
   its date — exactly the licensing/embargo semantics, with no separate mechanism.
6. **Crash-safe even inline.** The inline path takes the **same lease** as the worker; if
   the gateway dies mid-process the row is left `running` and the worker's expired-lease
   requeue reprocesses it. `AddMembers` is additive per `(segment, user)` so reprocessing
   is idempotent.
7. **Multi-replica** via `SKIP LOCKED`; the worker (in `cmd/pipeline`, reusing its
   onboarding wiring) can run N replicas.

## Consequences

**Positive**
- One ingestion pipeline, one processor, one status table — the sync/async split and the
  row cap are gone.
- Every accepted upload is durable + recorded + retried; nothing is lost on a crash.
- Scheduling (`hold_until`) is a column, not a subsystem.
- Matches how DMPs/CDPs actually work (async jobs with status + match-rate).
- The onboarding-monitor gains live `queued`/`running` visibility, not just terminal rows.

**Negative / cost (accepted)**
- **Match rate for large files goes async.** For files above the inline threshold the
  advertiser sees "processing…" then a result in the monitor / status endpoint (seconds
  for anything near the threshold). *Mitigation:* cheap validation (format, empty,
  oversize) stays synchronous at upload; only *matching* is deferred; small files stay
  inline.
- **Files must be staged to S3 even for inline** (so a crashed inline job is recoverable
  by the worker). Small cost, uniform recovery.
- **Monitor migration.** Folding `onboarding_runs` means the onboarding-monitor UI
  repoints to `audience_ingest_jobs`.

## Migration plan (phased — each shippable + reversible)

1. **Queue + processor + worker.** Add `audience_ingest_jobs` + `pkg/ingestjobs` (copy
   `pkg/reportjobs`); refactor the pipeline onboarding stages into `processStagedFile`;
   add the ingest worker loop in `cmd/pipeline`; switch the **drop-zone poller to
   enqueue**. 1st-party path unchanged. Verify via the existing drop-zone e2e.
2. **Gateway → stage + enqueue/inline.** Upload does synchronous validation → stages the
   file → inserts a job → inline-processes if small+due-now (200) else 202; add
   `GET /v1/api/audiences/ingest/{id}`; delete `runAudienceUpload` inline + the row cap.
   Update the portal + any e2e asserting the synchronous response.
3. **Scheduling.** Expose `run_at`/`hold_until` on the upload API + monitor; document it's
   for licensing/embargo (go-live is still flight dates).
4. **Fold `onboarding_runs`.** Migrate the onboarding-monitor to `audience_ingest_jobs`;
   drop `onboarding_runs`.

## Open questions (resolved)

- **Fold `onboarding_runs`?** Yes — the job row already carries every count it had.
- **Keep a synchronous fast path?** Yes, but as an *execution mode* of the shared
  processor (inline vs worker), not a second code path — small+due-now runs inline.
- **Worker home?** `cmd/pipeline` (reuse the onboarding wiring; N-replica safe via
  `SKIP LOCKED`) rather than a new `cmd/ingest-runner`.
