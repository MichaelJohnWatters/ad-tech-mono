> **DONE 2026-10-15 (session 3): BASELINE PINNED to 8af405c6 at 110rps/30m**
> (clean + lossless: fill 95.4%, VERIFY GREEN, impressions = wins 188,819,
> canary -$0.09, sched p99 9.1ms, JetStream max 2.2k; fanout p50/p95
> 46.6/223ms). NOT 150rps: 150 sustained now saturates this VM (sched p99
> ~90ms within 3 min). Demo number: **~175rps sustained, lossless, full
> observability** (250 offered -> 176.8 achieved, latency-bound). Session 3
> shipped profiling tooling, router reseed fix, batched Redis reads, tmax +
> per-channel deadlines, reporting dedup pipelining + dsp_call republish
> dedup — details in "Session 3" below. Autoscaling RE-ENABLED at the end.

> **PROGRESS 2026-10-08 (session 2): root cause REFINED to a hard deadlock
> and lever 1 FIXED IN CODE (not yet deployed/measured).** WinRateCurve
> held RLock while calling t.Stats (second RLock) — with a writer waiting,
> that's the documented sync.RWMutex recursive-read deadlock: the tracker
> wedges PERMANENTLY, all later readers queue forever (6,090 caught), pod
> OOMs. Goroutine dump shows the full triad (reader-in-reader at
> shading.go:193←279, writer at :141, 6,090 queued). Fix shipped in
> pkg/bidshading: inline midpoint (no recursive lock) + per-placement
> lock-free curve cache invalidated on write; regression test
> TestTracker_ConcurrentReadWrite wedges old code in <15s, passes new in
> 1.5s with -race.
>
> **A/B MEASURED 2026-10-08 19:15 (110rps/10m, fresh world, HPA pinned,
> VERIFY green, lossless): fill 22.2% → 94.7%** (golden ~92);
> dsp-internal restarts continuous-OOM → **0**, peak RSS 499Mi → **97Mi**;
> DSP campaign_loop p95 3.3ms; exchange fanout p50/p95 28.8/240.6ms (at
> the scenario-market baseline 27.7/225).
>
> **GOMEMLIMIT shipped (e6aaeab5) + CEILING RAMP 2026-10-08 20:30**
> (150/200/250/300 × 5m, 3 pinned dsp pods, no HPA, one world across
> stages so fill decays with budget — by design): ALL stages held offered
> rate, 0 errors, lossless, 0 restarts. Fanout p50/p95 per stage:
> 150→24.5/573 · 200→392/938 · 250→732/973 · 300→747/975ms. Peak dsp RSS
> 464Mi — GOMEMLIMIT held the 300rps stage in the GC-pressure zone
> (396 goroutines, no pile-up, no OOM: the backstop works). Verdict:
> throughput/money ceiling is ≥300rps on this VM; the LATENCY knee is
> between 150 and 200 (p50 jumps 24.5→392ms). Caveat: 250/300 stages ran
> budget-depleted (mostly no-bid workload) — a fresh-world run at 200
> would be the clean knee measurement. Numbers NOT in the perfbench
> ledger (ramp run directly, not via perfbench.sh).
>
> **PG levers + knee re-probe (2026-10-08 evening):** grant skip-set
> (08c93265) + burl batching (524f4972) cut postgres 0.54→0.36 cores at
> 150rps. Fresh-world 200rps/5m probe: fanout p50/p95 **173/850ms at
> 85.8% fill** vs the ramp's 392/938 at 38% fill — p50 2.3× better on a
> harder workload, postgres 1.08→0.41 (−62%) at the knee. The clean knee
> now sits ≈200rps; remainder is the kernel floor (host-structural).
>
> **Lever 3 SHIPPED + LIVE-VALIDATED: in-flight bid cap**
> (dsp.max_inflight_bids, default 256, live; shedding.go wraps the shared
> handler so HTTP + gRPC shed under one budget; 204 no-bid, never queue;
> adtech_dsp_bids_shed_total). Validation: 0 sheds at default under
> 150rps; knob dropped to 2 live mid-run → ~1,300 sheds across 3 pods,
> 0 errors, 0 restarts; knob restored → sheds stop. A future pile-up now
> degrades to lost auctions instead of OOM death. REMAINING: cleanup tier
> (lever 4: invalidate storm, pools, hot-path logging, exchange
> Transport), burl claim batching + delivery investigation (PLAN),
> autoscaling left OFF by choice.

# Session: DSP OOM crash-loop — the shading-tracker lock convoy

## Context

Post VM-restart cleanup (previous session) left one mystery: dsp-internal
still averaged ~231ms / ~41% timeouts at 110rps with ~6% CPU, read as
"blocked on I/O — pprof it". This session ran the pprof hunt. The I/O
framing was wrong: the time goes to lock wait, and the latency/timeout
numbers are largely an artifact of the pods being repeatedly OOM-killed
mid-run. Block profiling kept dying with the pods (armed rates reset on
every restart — that itself was the tell); the goroutine profile is what
nailed it.

## Root cause (verified, don't re-derive)

The chain, each link observed on 2026-10-08 under `make loadtest RPS=110
DURATION=10m` (73-campaign big-world, HPA pinned off, warm caches):

1. **Per-candidate curve rebuild under RLock.** The bid loop calls
   `shadingTracker.WinRateCurve(placementID)` per shading-enabled candidate
   (`cmd/dsp/main.go:1291`). `WinRateCurve` (`pkg/bidshading/shading.go:239`)
   holds `t.mu.RLock()` while iterating up to `maxRecordsPerKey = 2000`
   records and building bucket maps. Bid requests carry ~79 candidates
   (see `"candidates":79` in no-bid logs) → up to ~160k record-iterations
   + map allocs per request, under the lock.
2. **Writer stream.** `recordWin`/`RecordLoss` take the full `Lock()` per
   win/loss notice (`shading.go:141,153`) — hundreds/sec at 110rps.
3. **Go RWMutex blocks NEW readers while a writer waits** → convoy.
   Observed: goroutine profile with **6,090 of 6,297 goroutines** in
   `sync.(*RWMutex).RLock` at `main.go:1291`, +828 in `Mutex.lockSlow`.
4. **Stuck handlers pile up unboundedly.** The exchange abandons the call
   at bid_timeout (500ms) but RLock ignores ctx cancellation — the gRPC
   handler goroutine stays parked, pinning stack + HTTP/2 stream + bufio
   buffers. Observed per-pod memory ramp 123→183→313→499Mi in ~3-4min
   while live heap stayed ≤~110Mi; heap diff growth is connection-flavored
   (grpc operateHeaders, bufio.NewReader/WriterSize) + ~6MB retained via
   `preloadIdentityResolver.ResolveIdentity`. `/proc/net/sockstat`: 3,438
   allocated TCP socks vs 18 in use.
5. **512Mi limit, no GOMEMLIMIT/GOGC set** on the deployment → cgroup
   OOMKill (`lastState: OOMKilled, exitCode 137`), all 3 dsp-internal pods,
   repeatedly (restart counts 3-4 within the hour). Each death =
   connection-refused at the exchange + cold warm-caches on rebirth.
   **This is the observed "41% timeout / 231ms / 22% fill (vs 92%
   baseline)"** — death-cycling, not store latency.

## Secondary findings (real, but not the killer)

- **advertiser_balances invalidate storm**: 779 full reloads in 5min on one
  pod (`warm cache invalidate received, reloading`). Publisher throttles
  5s/account (`cmd/reporting/balance_sink.go`) but ~33 spending advertisers
  ≈ one fleet-wide reload every ~1-2s/pod. The warm cache's targeted
  single-id path (`pkg/cache/warm/warm.go:317`) never fires for this cache.
  Explains the "bulk refresh context deadline exceeded" noise together with:
- **Tiny DSP pg pools**: `MaxOpenConns: 5` (`cmd/dsp/main.go:659,701`),
  `MaxOpenConns: 3` for balance (`cmd/dsp/balance.go:290`).
- **Exchange DSP HTTP client uses default Transport** →
  `MaxIdleConnsPerHost=2` (`cmd/exchange/main.go:126`) — connection churn
  on the 4 HTTP competitor legs. (Simulator already sets 4096; exchange
  never got the same treatment.)
- **Hot-path INFO logging**: 2-4 structured INFO lines per bid request
  ("bid submitted"/"no bid" per campaign) — alloc + stdout-pipeline cost
  (see 06-kernel-floor: per-log-line kernel cost is already a known tax).

## Ruled out (don't re-chase)

- Audience L1 cache growth — 3s absolute TTL + 5s janitor prune
  (`pkg/audience/store/l1/l1.go`), bounded by working set.
- Identity resolution I/O — preload resolver, in-memory expansion.
- Kernel socket memory — cgroup `memory.stat` shows `sock 0`; growth is
  all `anon` (process memory).
- Store latency as the primary cause of the 231ms — it's lock wait + death
  cycling. (Pool sizing still worth fixing, above.)

## BASELINE NOT PINNED — host VZ degradation (2026-10-08 late)

Tried to pin a golden 150rps/30m soak post-levers; could NOT get a clean
run. Sequence: a 350rps perfbench stage SEIZED the VM (apiserver + docker
socket died — DO NOT sweep >=300rps locally, the host runs out of cores
for its own kubelet before the app fails). After that, Redis command
latency spikes to 50-300ms UNDER LOAD (idle it's a clean 0.08us/<1ms) →
DSP audience phase (per-auction Redis read) pins at the 500ms bid_timeout
→ internal leg ~296ms/33% timeout → fanout p50 climbs to 400-730ms. This
is NOT the code: campaign_loop (the shading fix) stays 12.9ms, CPU/mem
fine, 0 sheds, and each soak STARTS clean (3-min fanout p50 18.3ms —
better than the 27.7ms scenario-market baseline, proving the levers
deliver) before degrading minutes in. The degradation SURVIVES both an
rdctl VM restart AND a full stack-down/stack-up — it's host-level VZ-
framework damage from the seizure, not a k8s state. stack-doctor reports
"healthy" (it checks API/pods/ledger, not Redis latency under load) — a
false green for perf. TO PIN THE BASELINE: full Mac reboot (surest) or a
long host cooldown, THEN confirm Redis stays <1ms under a 2-min load
probe BEFORE committing to the 30m soak, then pin BASELINE to 1755d082.
The post-fix numbers we DID capture on a healthy host earlier: 150rps/4m
fill ~95% fanout p95 208ms; 200rps/5m fanout p50 173ms @ 85.8% fill.

## Where the next gains live (attributed 2026-10-08 evening)

CPU-by-pod at the ramp's 200rps stage: **postgres-0 = 1.08 cores, the #1
consumer in the namespace** (5× any app pod; dsp ~0.19ea, exchange 0.08ea).
Control built into the ramp: at the budget-depleted 300 stage postgres fell
to 0.15 while DSPs tripled → **postgres load is WIN-driven**. Node modes:
200rps = 8.4/10 cores used (kernel ~4.8 structural, user 3.6); 300rps =
9.5/10 (user 5.6). Observability pods are NOT the lever (promtail 0.13,
loki/jaeger/grafana below top-14) — handoff 06's sampling fix holds.

Attribution (fresh world, 150rps/4m, 95.1% fill, pg_stat_user_tables diff
+ pg_stat_activity sampling at ~0.6s):
- **marketplace_grants JOIN** (data-fee attribution): ~28% of sampled PG
  time, ~40 idx-scans/s — PER WIN, against a table with 18 rows that
  changed 0 times during the run. A warm cache erases it.
- **burl_pending lifecycle**: ~40%+ of sampled time — INSERT ON CONFLICT
  per win, DELETE..RETURNING per burl fire, periodic created_at sweep
  DELETEs. ~10k inserts + 42k idx scans per 4min. Candidates: Redis with
  TTL (it's a pending-notification dedup), or batch the inserts.
- data_fee_pending point reads ~8%; identity_graph ingest ~3%.
- Catalog N+1 reload churn (insertion_orders/targeting_rules/accounts,
  ~55 seq-scans/s of tiny tables) + advertiser_balances invalidate storm
  (~2 full reloads/s): visible in counters, minor in time share.

Estimated recoverable: ~0.7–0.9 of postgres's 1.08 cores at the knee, and
it SCALES WITH WINS — at 250rps full-fill it would be proportionally
larger. Next concrete levers, ranked: (1) warm-cache marketplace_grants,
(2) burl_pending → Redis/batched, (3) the existing cleanup tier.

## Fix plan (ranked, A/B each per perf-loadtest protocol)

1. **[DONE IN CODE 2026-10-08] Deadlock fix + curve cache.** (a) Recursive
   RLock removed: midpoint computed inline via statsFromRecords instead of
   t.Stats() under the held lock. (b) `t.curves sync.Map` caches the built
   Curve per placement; RecordWin/RecordLoss invalidate after append;
   WinRateCurve is a lock-free Load on the hot path. Known-benign race: a
   rebuild racing an invalidation can re-store a curve stale by one record
   (bounded by the next notice). Regression guard:
   TestTracker_ConcurrentReadWrite. NOT yet deployed or A/B'd.
2. **GOMEMLIMIT ≈ 450MiB** env on dsp-internal (chart) — GC defends the
   cgroup limit instead of the OOM killer. Cheap resilience regardless of 1.
3. **Load-shed instead of queueing**: cap in-flight bid handlers
   (semaphore → instant no-bid when full, or grpc MaxConcurrentStreams).
   A bidder must answer inside tmax or say no fast; 6k-deep queues are the
   worst outcome. Also makes any future convoy degrade gracefully.
4. Cleanup tier: id-carrying invalidates + SingleLoader for
   advertiser_balances; raise DSP pg pools; demote per-bid INFO→Debug
   (or extend LOG_SAMPLE_RATIO to the DSP bid path); exchange Transport
   `MaxIdleConnsPerHost` (match simulator's 4096).

## Measurement notes for the next session

- Baselines: golden 0821d1c (auction p95 58.8ms, fill ~92%); DSP internal
  leg avg 22.6 / p95 76 (skill doc). Current broken state for comparison:
  2026-10-08 run = fill 22.2%, 0 errors, pipeline lossless, all-pods
  OOM-cycling.
- Block/mutex profiling: arm via `/debug/pprof/rates?block=10000&mutex=5`
  (kubectl exec wget, port 8082) — but remember arming DIES WITH THE POD;
  under OOM-cycling conditions use goroutine profiles + 45s-interval heap
  snapshots (monitor loop) instead. Snapshot files from this session:
  /tmp/dsp-heap-seq-*.pb.gz, /tmp/dsp-goroutines.txt (laptop /tmp,
  ephemeral).
- Helm: release `adtech` is in namespace **default** (resources in
  `adtech`) — `helm upgrade ... -n default`. Autoscaling was turned OFF
  this session (`--set global.autoscaling=false`); **re-enable after the
  fix session's final measured run** (`--set global.autoscaling=true`).
- World state after this session: budgets largely depleted (two 10m runs);
  reset + reseed before any comparable run.

## Session 3 (2026-10-14/15): pin, profiling, and what it found

Commits: f4fc3357 d13682a3 f1fb0e15 a5999c09 (pushed); 8af405c6 + BASELINE
pin + this doc (LOCAL — owner asked to keep later work unpushed).

### Tooling
- `scripts/perfprofile.sh`: CPU / block+mutex delta (armed only for the
  window) / heap / allocs / goroutines from EVERY pod of every hot service
  via kubectl exec; perfbench runs it at mid-run (PROFILE=0 to skip) into
  `perf-archives/<stamp>/pprof/` + SUMMARY.txt. **GOTCHA: summarise AFTER
  the load** — go tool pprof on the host mid-run took fleet sched p99
  ~5 -> ~100ms (run 2026-10-14 22:54, annotated contaminated).
- Ledger gained: per-bidder leg p50/p95/timeout%/skip%, per-channel
  fill/deadline/timeout%, Redis CPU/throttle, JetStream pending
  max/end/top, L1 hit %, DSP audience p50, SSP segments p95.
- perfbench push opt-out: `git config perfbench.push false` (set in the
  owner's clone) or PERFBENCH_PUSH=0.
- New metrics: `adtech_exchange_dsp_leg_duration_seconds{dsp_endpoint,
  channel,outcome}`, `adtech_exchange_fanout_deadline_seconds{channel}`;
  `adtech_auctions_total{channel}` is now the REQUEST channel (was always
  "all"). Panels: per-bidder leg latency + deadline-vs-timeouts (latency
  dashboard), Redis CPU + throttling (stack pressure).

### Findings + fixes (each unit-tested; e2e where cross-service)
- **Router reseed un-judge bug**: a lagging dsp_calls aggregate below
  MinCalls overwrote locally-condemned stats every 5s -> slowpoke at 43% of
  auctions for ~30s after each reset, ~20% for minutes post-boot. Reseed
  no longer un-warms a judged DSP. Live: ~1% from the first 15s.
- **Identity BFS** walked the whole depth-3 neighbourhood per bid (~48% of
  dsp-internal CPU) to return ids the caller discarded; stops at
  identity_max_linked (identical output, nearest-first).
- **Batched Redis reads**: DSP user + linked ids (up to 11 sequential
  round trips) and SSP user/household/hashed-email (3 goroutines) are now
  ONE pipelined call each, through the L1 cache (misses only) and probe.
- **Audience L1 cache A/B** (110rps): DSP audience p50 10.4 -> 6.8ms;
  p95 unchanged (~70ms, misses wait on Redis under load). Kept on.
- **tmax**: fan-out deadline = min(channel timeout, tmax - tmax_margin
  [25ms]); SSP stamps ssp.tmax (default 525ms = unchanged 500ms).
  **Per-channel**: exchange.bid_timeout_by_channel (default empty).
  e2e TestExchangeHonoursSSPTMax / TestExchangePerChannelBidTimeout.
- **Reporting consumer lag + dsp_call duplicates** — see the dedicated
  subsection below (a5999c09).
- Exchange marshals the bid request once per auction (was per leg).

### Reporting consumer lag fix (a5999c09) — full record

**Symptom.** The first 150rps/30m pin (2026-10-15 00:10, f1fb0e15) read
VERIFY RED: 113,684 impressions recorded vs 252,618 client wins, ledger
"fill 48.4%". Mid-run JetStream showed `reporting-optimise-dsp_call` 580k
pending, `reporting-events-impression` 144k, `audience-rt` behaviour 71k.
NOT loss: once drained, impressions = 253,374 unique (>= client wins).
VERIFY samples mid-drain, and perfbench's `fill_pct` then reads VERIFY's
impressions/auctions instead of the client's 95.0% — so a lagging run
looks like a fill collapse in the ledger. Check the drain before calling
it a regression.

**Root cause.** `cmd/reporting/batch.go` `batchProcess` (shared by every
core subject) did per-message dedup with a Redis SETNX **inside a serial
loop** — one round trip per message for the message-id layer, plus a
second for subjects with a business key (impressions etc.). A fetched
batch of N cost N-2N sequential Redis round trips before its single
ClickHouse insert, so consumer throughput ≈ 1 / Redis RTT, and under load
(Redis queueing behind a saturated node) it fell below the event rate.
dsp_call is the biggest stream (one event per DSP leg per auction).

**Fix.** Dedup is now two PIPELINED round trips per batch (message-id
keys for the whole batch, then business keys for the survivors):
- `events.BatchDedupStore` (MarkProcessedMany) + `events.MarkProcessedMany`
  helper with a serial fallback; `cache.SetNXBulk` (pipelined SETNX) on the
  Redis client, forwarded by `SelfHealingL2`; `L2DedupAdapter` implements it.
- Semantics unchanged: a pipeline executes in order, so a key repeated
  within one batch is "new" only at its first position — same as N serial
  calls. Order stays insert (durable) → bill → ack.
- **Failure safety (the subtle part):** on a Redis error the batch is
  nak'd (fail-closed, as before) and ONLY the keys this call newly set are
  released (`releaseNew`). Releasing every key would also delete
  pre-existing markers of genuinely processed messages and let their
  redelivery double-count; releasing none would make the retry look like
  a duplicate and drop it. The Redis pipeline reports per-command results
  even on partial failure; the contract is documented on the interface.

**dsp_call duplicates (found while verifying).** ClickHouse held 227k
byte-identical dsp_calls rows (~21% of the pin's rows), concentrated in
the heaviest-backlog window, with DIFFERENT stream sequences — i.e. the
exchange published the same event twice (spool / ambiguous-ack republish
under NATS pressure), which message-id dedup cannot catch. Impressions
already had a business key; dsp_call had `bizKey: nil` because a trace can
legitimately carry several calls per DSP (video pod sub-auctions). Key is
now `trace_id|dsp_endpoint|timestamp-ns` — a republish carries the
identical exchange-stamped timestamp, distinct calls never share one.
These dups inflated the router's reseed aggregate.

**Verification.**
- Unit (cmd/reporting/batch_test.go): pipelined path = exactly 2 dedup
  round trips per batch, never per message; partial pipeline failure
  releases only own keys (pre-existing marker survives) and naks all;
  dsp_call republish dropped while a distinct same-trace call lands;
  existing serial-fallback tests unchanged and green.
- Drain on the SAME backlog, measured from NATS `/jsz` (not Prometheus —
  its pending series was scrape-stale and briefly showed a bogus 16
  msg/s): old ~910 msg/s → new ~3,600 msg/s (ClickHouse ~4,000 rows/s).
- e2e green: TestFraudDedupSameImpressionDropped,
  TestTrackerRejectedEventDedup (+ the rest of the focused set).
- No duplicate impressions (rows = uniqExact(trace_id)).

**What it did NOT fix (and why).** At 150rps SUSTAINED the node saturates
(fleet sched p99 ~90ms); reporting (50m request) then gets ~0.1 cores and
the backlog still grows over a 30m run, draining after load stops (rerun
pin a5999c09: peak 1.03M, VERIFY RED mid-drain, lossless). Raising the
consumer requests to 200m did not help (see "Measured + rejected"). At
110rps (the pin) the consumers keep up: JetStream max 2.2k, VERIFY GREEN.
Remaining lever: cut dsp_call volume (sample, or one aggregate event per
auction) — it is most of the JetStream load.

**How to re-check.** During/after a run: `kubectl -n adtech exec nats-0 -c
nats -- wget -q -O- 'http://localhost:8222/jsz?consumers=true'` →
num_pending / num_ack_pending / num_redelivered per consumer;
`perfbench` ledger fields jetstream_pending_max/_end/_top. Duplicate
check: `SELECT count() - uniqExact(trace_id, dsp_endpoint, channel,
latency_ms, timed_out, bid_price_usd, timestamp) FROM dsp_calls`.

### Measured + rejected (don't redo)
- **Consumer CPU request 50m -> 200m** (reporting, audience-rt) at
  150rps/10m: backlog peak WORSE (352k vs 254k), serving + sched flat
  (~90ms both). The node is saturated; a request only reshuffles a fixed
  budget. Reverted.
- **Redis is not the bottleneck**: 0.11-0.14 cores at every rate, no
  throttling; freqcap tail tracks fleet sched p99 (node saturation).
- adserver freqcap is already ONE Redis round trip per serve (Lua) —
  nothing to batch.

### Demo ramp (2026-10-15, 5m/stage, fresh world, full observability)
150/200/250 offered -> 148.8/169.3/176.8 achieved; all VERIFY GREEN,
0 errors, fill 95.3/94.5/94.1%, canary <= $1.7. Throughput is
latency-bound (simulator's 300 workers / ~1.5s requests), not failing.
Healthy-bidder timeouts 2% -> ~20% at 250 (VM contention past 500ms).
JetStream drained to 0 after each stage. Present ~175rps as the demo
number; show latency at 110rps (baseline).

### Per-channel timeout A/B (110rps/10m)
Control (empty, 500ms everywhere) vs display=250ms,native=250ms,
video/ctv/audio=600ms: fill 95.5 vs 95.4% (display 96.54 vs 96.55%),
fanout p50/p95 53/232 -> 48/220ms, SSP auction p95 249 -> 242, render
136 -> 116ms, display/native leg timeouts 0.25 -> 0.67%. Small real win
at normal load (mostly: the slowpoke's 1% probes cut at 250 not 500).
NOT made default: under saturation (200+rps) healthy bidders' p95 reaches
~700ms, so 250ms display would cut 20%+ of bids. Key reset to empty;
revisit on staging hardware (or ~300ms display) — owner's call.

### Gotchas recorded this session
- The helm release had a FROZEN full values snapshot as user-supplied
  (since an Oct 8 `--reuse-values --set`), silently ignoring chart edits.
  Upgrade with `helm upgrade adtech k8s/helm/adtech -n default --set
  global.autoscaling=<bool>` (no --reuse-values); diff `helm get values`
  first if unsure.
- kubelet image GC evicted adtech-migrate (stuck post-upgrade hook):
  `docker builder prune -f` (67GB cache) + rebuild Dockerfile.migrate.
- `redis-cli --latency` inside the pod reads healthy while clients see
  queueing — judge Redis by DSP audience / freqcap phases + sched p99.
- Prometheus `jetstream_consumer_num_pending` is scrape-stale for short
  windows; measure drain via NATS /jsz (ack_floor / num_pending) instead.
- TestSchemaSizes dsp count was stale since 1755d082 (fixed).

### Next levers (attributed)
1. Saturation knee ~150rps is CPU: infra floor + per-request work. The
   per-bidder legs at 200+ show the competitor DSPs' own tails dominate.
2. dsp_call volume (1 event per leg per auction) is the biggest JetStream
   stream — sample it or aggregate per auction to cut pipeline load.
3. Staging hardware (handoff 03) for a real high-RPS number.
