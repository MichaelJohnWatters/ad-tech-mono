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
> the scenario-market baseline 27.7/225). REMAINING: GOMEMLIMIT (lever 2),
> load-shed (lever 3), cleanup tier (lever 4), re-enable autoscaling.

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
