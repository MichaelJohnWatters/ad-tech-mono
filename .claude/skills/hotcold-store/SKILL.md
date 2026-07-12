---
name: hotcold-store
description: Stand up and validate the hot/cold analytics store end-to-end (ClickHouse hot + DuckDB/Delta-lake cold), then prove zero data slippage across the hot→cold boundary. Use when asked to enable the cold store, run the hot/cold-store long-run test, or verify deep-history (cold) reads work against the live stack. Automated version: make test-e2e-hotcold (TestHotColdStore).
---

# Hot/cold-store routing: bring-up + end-to-end validation

Proves the `HotColdStore` for real: recent data reads from **ClickHouse (hot)**,
aged data from the **Parquet/Delta lake (cold, via DuckDB delta_scan)**, and a
range spanning the boundary is split + merged losslessly. Hot/cold storage is **off by
default** (`reporting.cold_store_enabled=false`) and needs a duckdb-tagged reporting
image, so this is a deliberate setup, not a flag flip.

## Key facts (why the setup is what it is)

- **Data isn't "moved" hot→cold.** Reporting writes ClickHouse and the **pipeline**
  writes the lake — both consume the same NATS stream independently (dual-write).
  `hot_window` is a *read-routing* boundary, not a mover. Nothing ages data on a
  schedule; ClickHouse has no TTL, so it keeps everything.
- **The pipeline (datalake sink) must be running** — it populates the cold store.
  It is NOT in the default stack; this skill adds it.
- **Reporting needs the `duckdb` build tag** for cold reads. go-duckdb does NOT
  link on Alpine/musl (the zig fast path), so use a **glibc** image
  (`build/Dockerfile.reporting.duckdb`).
- **Endpoint form:** Minio as `minio:9000` (host:port, no scheme) works for both
  minio-go (pipeline writer) and DuckDB delta_scan (reporting reader).
- **`NewParquetReader` registers a DuckDB S3 SECRET** — without it the delta ext
  hits EC2 instance metadata (169.254.169.254) and fails on Minio.

## Params

| Param | Test value | Notes |
|---|---|---|
| `REPORTING_HOT_WINDOW` | `15m` | Short so data crosses the boundary fast. Use `168h` for realistic. |
| `PIPELINE_DATALAKE_BUCKET` | `adtech-datalake-hotcold` | **Fresh** bucket = non-destructive, all real-Delta. |
| sim rps | `8`–`20` | Any steady load. |

## Prereqs

- Stack up (run the `local-stack` skill if ports refuse). ClickHouse backend
  (`REPORTING_ANALYTICS_BACKEND=clickhouse`). Tilt running.
- Keep the Mac awake for the window: `caffeinate -i -t 2400 &` (OrbStack suspends
  on sleep — see Recovery).

## Setup (already committed; these are the moving parts)

1. **`build/Dockerfile.reporting.duckdb`** — glibc build with `-tags duckdb`.
2. **Tiltfile** — reporting `docker_build` uses that Dockerfile; a `pipeline`
   resource is added (pure-Go build, deps nats+minio).
3. **`k8s/base/reporting/deployment.yaml`** — cold-store env: `REPORTING_COLD_STORE_ENABLED=true`,
   `REPORTING_HOT_WINDOW=15m`, `S3_ENDPOINT=minio:9000`, `S3_ACCESS_KEY/SECRET_KEY/REGION/USE_SSL`,
   `PIPELINE_DATALAKE_BUCKET=adtech-datalake-hotcold`.
4. **`k8s/base/pipeline/{deployment,service}.yaml`** — same S3 + bucket env, `PIPELINE_DATALAKE_FLUSH_INTERVAL=10s`.

Tilt auto-reconciles on save. If it doesn't: `tilt trigger reporting && tilt trigger pipeline`.

### Confirm bring-up
```
# reporting engaged hot/cold storage (NOT the hot-only stub):
kubectl logs -n adtech deploy/reporting | grep "hot/cold store enabled"
# pipeline sink is consuming + connected to Minio:
kubectl logs -n adtech deploy/pipeline | grep -E "datalake sink subscribed|object store connected"
```

## Validation (the zero-slippage proof)

**Automated:** `make test-e2e-hotcold` (`TestHotColdStore`, tag `e2e`) does this
whole proof — sets a 90s window, fires a burst, waits it out, asserts the cold
read + boundary merge, restores 15m (~3min). The manual procedure below is for
inspecting a specific window or debugging.

Cleanest design: fire a **bounded burst**, let it age past `hot_window` with **no
new traffic**, then a full-window query must be served **entirely from cold** and
still match the ClickHouse ground truth. Scope every query to a window BOTH stores
share (after the pipeline came up) — the lake bucket is fresh, so it won't have
pre-pipeline history that ClickHouse does.

```
CH="http://localhost:8123/?user=adtech&password=adtech-local-dev"
WT="<UTC time after pipeline start, before the burst>"     # e.g. 2026-07-12T10:50:00Z

# 1. Fire the burst; note wins.
go run ./cmd/simulator run --profile steady --requests 400 --rps 20   # -> Wins: W

# 2. Baseline (data still hot): hot/cold == CH ground truth.
curl -s "$CH" --data-binary "SELECT count() FROM adtech.impressions WHERE timestamp >= '<WT as YYYY-MM-DD HH:MM:SS>'"
curl -s -XPOST localhost:8086/v1/reporting/query -d "{\"table\":\"impressions\",\"metrics\":[\"count\"],\"time_from\":\"$WT\"}"
#    -> both equal (hot path).

# 3. Wait > hot_window (15m) with NO new traffic, then re-run the hot/cold query.
#    Now [WT, now] is entirely older than the boundary -> routes to COLD (lake).
#    Assert: hot/cold count == CH ground truth  (cold read is lossless).
#    Cross-check: a last-5m query returns ~0 (no recent traffic) -> proves the
#    count came from cold, not hot.

# 4. Merge proof: fire a small fresh burst (recent = hot). Full-window query
#    now returns cold(old) + hot(fresh) merged.
```

Also reconcile the two write paths: pipeline `/debug/datalake/snapshot?table=impressions`
`total_rows` should equal the CH count for the same window (dual-write agreement).

### Actual result (first live run, 2026-07-12, hot_window=15m)

- Cold read works: after aging, hot/cold `[WT,now]` routed to the lake and returned
  **352** (== lake snapshot), HTTP 200 — not ~0, so cold didn't silently fail.
- Merge exact: cold **352** + fresh hot **49** = hot/cold **401**; `last-5m` hot-only
  = **49**. Routing + split/merge provably correct.
- Steady-state lossless: the fresh 49-win burst landed 49/49 in BOTH stores.
- **Found + FIXED — the sink was at-most-once (lost buffered events on restart).**
  CH had 353 rows = 353 distinct trace_ids (no dup) but the lake had 352: one
  impression lost. Root cause: `datalakeSink` **acked the NATS message on buffer**
  (before flush), so events buffered-but-unflushed at a pod restart (an OrbStack
  sleep here) were acked yet never written. Fixed: ack-after-flush — each buffered
  event carries its ack/nak, ack only on a successful flush, NAK on failure so
  JetStream redelivers (at-least-once). Guarded by unit tests + this e2e test.

## Teardown / revert

- Restore the Tiltfile reporting block to the zig + `Dockerfile.dev` fast path
  (the duckdb in-image build is slower for normal dev).
- Set `REPORTING_COLD_STORE_ENABLED=false` (or remove the cold-store env) to return to
  hot-only. Keep the `pipeline` resource — it's a real component worth having.
- Delete the test bucket if desired (`adtech-datalake-hotcold`).

## Recovery (OrbStack slept mid-run)

Symptom: `dial tcp 127.0.0.1:26443: connect: connection refused`, pods vanish.
`orb status` shows `Stopped`. Fix (see also the `local-stack` skill):
```
orb start                                   # node returns Ready
# wait for nats-0 to be 1/1, then:
tilt trigger pipeline && tilt trigger reporting
# refresh warm caches on 8084/8081/8085/8082/8089/8090/8086 (POST /debug/cache/refresh)
```
