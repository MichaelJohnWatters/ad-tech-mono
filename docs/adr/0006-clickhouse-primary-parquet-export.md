# ADR 0006 — ClickHouse is the single analytical store; Parquet is an hourly export

**Status:** Proposed (2026-07-22)
**Supersedes:**
- ADR 0001's role split ("DuckDB = cold/ad-hoc query engine over the lake") — DuckDB
  drops off the read path entirely.
- ADR 0002's **live dual-write** into the Parquet lake — the lake is no longer a NATS
  consumer; it becomes a *derived export from ClickHouse*.
**Related:** ADR 0001 (analytics engines), ADR 0002 (raw-data pipeline), the
`docs/diagrams/data-lifecycle` write path, `docs/diagrams/data-reporting` hot/cold.

## Context

Today two stores consume the NATS event stream independently (dual-write):
- **reporting → ClickHouse** (hot: dashboards, rollups).
- **pipeline → Parquet/Delta lake on Minio** (cold: keep-forever archive + ML corpus).

Cold reads use **DuckDB `delta_scan`** over the lake; the **profile-builder** reads the
lake with a **pure-Go Apache-Arrow reader that loads whole partitions into memory**.

Two problems drove this decision:

1. **The profile-builder OOMs by design.** It pulls raw `behaviour_signals` /
   `profile_signals` rows into Go and aggregates in-process. Its `profile_signals` read is
   even **unwindowed** (loads the whole table). Memory scales with lake size → OOM at
   scale. (A 256Mi gateway that tried the same read was OOMKilled — the symptom that
   surfaced this.) The fix isn't "more memory" — it's "aggregate server-side."
2. **Two engines + a CGO tax.** DuckDB is CGO, which is why the reporting image is
   special-cased (in-image glibc build) and why the profile-builder *avoided* DuckDB
   (getting the Arrow OOM instead). Maintaining ClickHouse **and** DuckDB **and** a live
   dual-write is complexity we don't need at this scale.

Single-node analytical engines (ClickHouse) comfortably handle 100s of GB with server-side
aggregation and pushdown. We are nowhere near needing a distributed engine (Trino/Spark);
those are explicitly out of scope here (see "Escalation" below).

## Decision

**ClickHouse is THE analytical store. ALL analytical data flows into ClickHouse. The
Parquet lake becomes a derived hourly export.**

1. **Everything analytical lands in ClickHouse** — events (impressions/clicks/conversions/
   views), auction logs, **and** the profile-store tables (`behaviour_signals`,
   `profile_signals`). One ingest target, one query engine.
2. **The profile-builder queries ClickHouse** (server-side `GROUP BY` for rule evaluation
   and reconcile) instead of Arrow-reading the lake. The OOM ceiling disappears — only
   aggregated results cross the wire.
3. **The Parquet lake is a scheduled hourly EXPORT from ClickHouse**, not a live NATS
   consumer. It stays the **open, cheap, keep-forever ML corpus / archive** — but *derived*.
   (`INSERT INTO FUNCTION s3(...) FORMAT Parquet`, partitioned by `event_date`.)
4. **DuckDB drops off the read path.** Cold/archive reads are served by ClickHouse — either
   its long-retention hot tables or the `s3()` table function over the exported Parquet.
   DuckDB may remain a purely-optional ad-hoc tool, but nothing in the platform depends on it.
   This finally banks ADR 0001's `CGO_ENABLED=0` win everywhere.

## Consequences

**Positive**
- **The profile-builder OOM is gone** — aggregation is server-side; memory is bounded by
  the result, not the table.
- **One analytical engine.** Drop DuckDB from the build → no CGO special-casing anywhere.
- **No live dual-write.** The pipeline stops being a second NATS consumer racing its own
  Delta versions; the lake is a simple derived artifact.
- **Simpler mental model:** NATS (log) → ClickHouse (all views) → hourly Parquet (archive).
- The open Parquet ML corpus is **preserved** (still S3, still open format, still cheap).

**Negative / cost (accepted, with mitigations)**
- **The lake is now a MOVER, not a dual-write.** ADR 0002 deliberately made hot & cold
  independent so either could fail alone; an export re-introduces a dependency: the cold
  archive lags by ~1h and a failed export is a cold gap. *Mitigation:* the export is
  idempotent (overwrite the hour's partition), alertable (last-successful-export metric),
  and re-runnable; NATS replay still rebuilds ClickHouse, from which the export re-derives.
- **ClickHouse is now critical for all analytical data** (bigger blast radius). *Mitigation:*
  NATS JetStream remains the durable source of truth — ClickHouse is rebuildable by replay.
- **GDPR delete must reach the immutable Parquet archive.** *Mitigation:* delete in
  ClickHouse (lightweight deletes) **and** re-export / filtered-rewrite the affected
  partitions (the archive already needed a rewrite path; it's now export-driven).
- **`behaviour_signals` / `profile_signals` are new ClickHouse tables** (extra ingest MVs).

## Migration plan (phased — each phase is independently shippable and reversible)

1. **Land profile-store data in ClickHouse.** Add `behaviour_signals` + `profile_signals`
   ClickHouse tables + ingest (reporting consumes `behaviour.observed` / `profile.signal`,
   or MVs). Keep the lake dual-write for now — pure addition, no removal.
2. **Repoint the profile-builder to ClickHouse.** Rule evaluation + reconcile become
   `GROUP BY` queries against ClickHouse. Verify membership parity vs the lake path.
   **This is the phase that kills the OOM** — highest value, do it early.
3. **Repoint cold reporting reads to ClickHouse** (long-TTL tables and/or `s3()` over the
   export). Drop the `duckdb` build tag from reporting → `CGO_ENABLED=0` everywhere.
4. **Add the hourly ClickHouse → Parquet export job** (partitioned, idempotent per hour).
   The lake becomes derived.
5. **Retire the pipeline's live lake dual-write sink** and the DuckDB cold reader once
   1–4 are proven. GDPR delete propagation to the export lands here.

Phases 1–2 alone fix the presenting problem (OOM) and can ship without touching reporting.

## Open questions (resolve during implementation, not now)

- **Export vs ClickHouse S3 tiering.** ClickHouse can auto-tier old parts to S3 (storage
  policy). That's ClickHouse-internal format, though — *not* open Parquet. We keep the
  explicit Parquet export specifically because the **ML corpus must be open**. Revisit only
  if the open-corpus requirement changes.
- **Cold-read engine:** ClickHouse `s3()` over the Parquet export (drops DuckDB) vs keep
  DuckDB for ad-hoc. Lean: `s3()`, DuckDB optional.
- **Export exactly-once:** per-hour partition overwrite keyed on `event_date`/hour.
- **GDPR:** delete-in-ClickHouse + partition re-export vs a filtered-rewrite job over the
  archive.

## Escalation (explicitly NOT now)

If a single ClickHouse node ever genuinely can't hold a query's working set: scale
ClickHouse (shard/replicate) or reach for **Trino** over the lake — **not Spark**, which is
a heavy JVM batch cluster at odds with the local-first, tiny-binary ethos. Spark's only
plausible home is a future **Python ML** batch pipeline over the open Parquet corpus,
decided on its own merits. See the "scaling ladder": bound reads → push down → bigger node
→ distributed SQL → (maybe) Spark-for-ML.

### On "one ClickHouse to write, one to read" (replication)

A recurring instinct is to split ClickHouse into a write node and a read node. Worth
recording why that is **not** the next step:

- ClickHouse replication (`ReplicatedMergeTree` + Keeper) is **symmetric multi-master**,
  not Postgres-style primary/standby. Every replica holds a full copy and serves both
  reads and writes; "write here, read there" is a routing convention you impose at the
  LB/app layer, not a DB role. Replication is async (sub-second typical).
- It buys **read/write resource isolation**, **read concurrency**, and **HA** — but it does
  **not** shrink a single heavy query (each replica runs it on its own CPU/RAM). The
  profile-builder OOM was a single-query working-set problem; replication would not have
  helped — server-side aggregation (phase 2) did.
- Cheaper moves come first on the ladder: **phase 4's Parquet export already gives read
  offload** (heavy cold/ad-hoc/ML reads hit `s3()`, not the live ingest node), and a
  **bigger single node** absorbs contention with far less operational surface than a
  Keeper quorum + two full copies.
- The one strong standalone reason to replicate is **HA** (ClickHouse is currently a SPOF
  for analytics). When that's the goal, prefer **compute/storage separation** (stateless
  compute over shared S3, the ClickHouse-Cloud model) over two full-copy replicas.

Verdict: keep replication in the back pocket as the **HA** move; it is not a scaling step
for this workload and is explicitly out of scope now.
