# profile-builder (Job)

One-shot batch expansion engine for the profile store: cluster `identity_graph`
into persons (union-find → `identity_clusters` in PG + a Delta lake artifact),
evaluate behavioural rules into segment memberships (person-level enroll,
expanded to every id in the cluster, replace-by-segment prune), expand plain
onboarded segments across clusters, and reconcile `profile_signals` back into
PG memberships. Thin wrapper over `pkg/profilebuilder.Run` — all logic lives
there; this binary just wires config/DB/lake/ClickHouse.

## How it runs

- **Normally NOT run directly.** The hourly `batch-conductor` CronJob
  (`k8s/helm/adtech/values.yaml` → `cronjobs.batch-conductor`, `10 * * * *`)
  calls `pkg/profilebuilder.Run` in-process as a chain step (`pkg/batch/chain.go`).
  This binary is the standalone escape hatch (manual / offline dev).
- One-shot: run → log `Result` counts (clusters/enrolled/pruned/expanded/reconciled) → exit. 10-minute ctx timeout.
- Idempotent: memberships are recomputed wholesale each run (replace-by-window); a retry just recomputes.

## Wiring + degradation (deliberate, logged)

- **Postgres required** — exits 1 if unreachable.
- **Lake optional** — S3 (`s3.endpoint` etc.) or `/tmp/adtech-datalake` fs fallback; nil = clustering-only run.
- **ClickHouse (`profile_builder.clickhouse_addr`)** — ADR 0006 phase 2: rule
  evaluation + reconcile via server-side CH GROUP BY (`NewCHBehaviourQuerier`,
  the OOM fix). Empty/unreachable addr **silently falls back to in-Go lake
  reads** — in-cluster this MUST point at `clickhouse:9000`, not the localhost
  default (see the helm values comment).
- **NATS optional and vestigial** — `Config.Bus` is wired but the builder
  publishes NO invalidates: membership freshness rides the migration-078
  `audience_segment_members` changelog trigger + the pipeline's single drainer.

## Gotchas

- Cross-tenant by design: segment reads use the RLS platform-read hatch
  (`platformReadTx`). A wrong DB role silently blanks reads — the
  zero-rule-segments WARN at the end of `Run` exists because exactly that
  happened (hourly runs reported `enrolled=0` for days). Investigate that WARN.
- Config keys in `pkg/config/keys/profilebuilder.go` (`profile_builder.*`:
  nats_url, datalake_bucket, min_confidence, max_cluster_size, clickhouse_*);
  Raw keys, nil schema — no live-config polling.
- `MaxClusterSize` drops oversized clusters as linking pathology (shared
  device would smear one person's segments over strangers).

## See

- `docs/PLAN.md` → "Profile Store (Normalized Signals → Expansion → Memberships → Export)", "Identity Graph", "Segment Building Pipelines"
- `docs/AUDIENCE-PIPELINE.md` (end-to-end freshness path), `docs/adr/0006-clickhouse-primary-parquet-export.md` (why CH reads)
