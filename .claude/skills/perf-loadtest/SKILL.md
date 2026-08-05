---
name: perf-loadtest
description: Run a clean, comparable performance load test of the auction path (reset → seed → warm → loadtest → verify → read phase metrics) and A/B a change against recorded baselines. Use when asked to measure auction latency/throughput, validate a performance fix, run a soak, or "run the load test protocol". Covers the money invariant (VERIFY), the overspend canary, and the host hands-off rules that make runs comparable.
---

# Performance load-test protocol (auction path)

The point is **comparable, trustworthy numbers**: every run starts from the
same world, warms the same caches, holds the host steady, and checks the money
invariant. A run that skips a step produces numbers that can't be compared to
the baselines — worse than no run.

## Protocol (in order, no skipping)

1. **Fresh world** (fill is NOT comparable across budget-depletion states):

       make reset
       go run ./cmd/seed --profile standard --big-world-advertisers 20 \
         --big-world-publishers 5 --big-world-campaigns-per 3

   That's the 73-campaign world (13 standard + 60 big-world) with the premium
   bid stratum and budget spread the baselines assume. The seed also plants the
   **overspend canary** advertiser "Canary Low-Balance" at $5.

2. **Warm caches** — `go run ./cmd/seed` does NOT broadcast invalidates:
   `POST /debug/cache/refresh` on ports 8081 8082 8084 8085 8088 8089 8090
   **AND directly on every dsp-internal pod** (`kubectl exec … wget`) — the LB
   only reaches one pod.

3. **Load** (VERIFY is the money invariant and must stay green):

       make loadtest RPS=110 DURATION=10m VERIFY=1

   Stay at **≤110 rps for latency work** — 150+ saturates the 8-vCPU VM (23%
   irq) and you're measuring the VM, not the code. 200+ needs an
   `rdctl` cpu/memory bump (known VZ-restart wedge risk).

4. **Read the results:**
   - Grafana **"Phase profiling"** row (exchange gates/routing/fanout/dealeval/
     finalize; DSP parse/audience/campaign_loop/encode; SSP pre_auction/
     pre_auction_segments/auction/render; adserver freqcap/creative/assemble)
     + **"Per-pod"** row (a sick replica can't hide in an average).
   - **"Stack pressure"** row — the is-it-code-or-is-it-the-VM discriminator:
     Go scheduler latency p99 by service, fleet CPU/RSS by service, node CPU
     by mode (iowait = thrash, irq ~23% = virtio ceiling), node MemAvailable
     (~1.5GiB = page-cache-thrash danger zone). If these climb with your
     phase p95s, the fix is cores/replicas, not code.
   - pprof during load: `/debug/pprof/profile?seconds=20` on 8081/8082/8084.
   - After the run: consumer drain (`jetstream_consumer_num_pending` → 0),
     spool counters (spooled == drained, dropped == 0).
   - **Canary**: "Canary Low-Balance" must deplete $5 → ~0/slightly negative;
     its negative depth = measured balance-gate precision (bound: cents).

5. **A/B every fix** against the recorded baselines; commit each with
   before/after numbers in the message.

## Baselines (110rps, warm, 73-campaign world, 10m steady-state)

Post auction-speed push + competitor CPU rebalance (2026-08-05, fd4e471):

- Exchange: **fanout p95 208ms** (p50 ~60); other phases ≤2.7ms
- SSP: pre_auction p95 ~50-63ms (p50 8.9) | auction p95 **231ms** (p50 58.5)
  | render p95 110-160
- DSP: parse ≤5.6 | audience p95 **46.5** | campaign_loop p95 ~1ms | encode
  0.5; internal leg avg 22.6 / p95 76ms; competitor legs avg 35 / p95 ~118ms
- Adserver: freqcap p95 80ms (← next target: serial cap Redis ops) |
  creative 1.0 | assemble 2.4
- Fill 92.0%, 0 errors, money-lossless; canary depth −$0.06 (10m); sched
  latency p99 ~8-10ms on the auction path
- 30-min soak reference: fill 82.1% sustained (demand exhaustion), canary
  drifts ~1.4¢/min post-depletion (rebase optimism window)

Pre-push reference (HEAD 1e4af7f): SSP pre_auction p95 237ms / auction 348;
DSP campaign_loop p95 68.6ms, audience 75.9; fanout p50 91ms; competitor
legs p95 ~320ms; 30-min soak 83.7% fill.

Block/mutex profiling: arm via `/debug/pprof/rates?block=10000&mutex=5` on
the pod's internal port (kubectl exec wget), capture /debug/pprof/{block,
mutex}, disarm with ?block=0&mutex=0.

## Hands-off host rules (violations invalidate the run)

- **Nothing on the host mid-run**: no builds, no npm/docker, no browsers — an
  `npm install` once collapsed fill 97→12% (simulator + VM share the cores).
- **Never deploy ANYTHING mid-run** — even a configmap-only helm upgrade
  restarts prometheus/grafana → probe-kill cascade on the saturated VM.
- Build + deploy everything BEFORE the run; `docker system df` if pulls fail
  after heavy building (kubelet imageGC at 85% disk eats local images).

## Trust rules

- `dsp_calls` telemetry rows are lossy under NATS pressure — ClickHouse
  auctions/impressions tables are ground truth; graphs can lie.
- TB runs `--development` locally: batch caps are learned adaptively
  (pkg/billing/tigerbeetle); "Maximum batch size exceeded" per-entry fallback
  = regression, not noise.
- Post-run fill attribution is queries, not logs: `dsp_calls.no_bid_reason`
  (blank = the exchange-side call FAILED — a signal), `freq_cap_blocks
  GROUP BY household_id`.
- Chaos variant: `make test-e2e-chaos` (incl. NATS-outage spool losslessness).

## Market composition (since caeba06)

The competitor fleet is a SmartRouter scenario bench: competitor1 =
slowpoke (500ms delay → timeout-skipped minute 1), competitor2 = deadbeat
(97% no-bid → bid-rate-skipped), competitor3 = the real competitive market
maker (:8098). Expect dsp_calls to show comp1/comp2 at the ~1% ε-probe
trickle after warm-up — that's the router working, not a broken DSP.
Fanout p95 baselines predating this reflect a market where slow legs were
always waited on.

## Iron rule (thrice-proven)

**Nothing in the per-campaign bid loop may do per-call network I/O.** Budget
reads, balance reads — all in-process copies kept warm by background bulk
refreshers. Catalog growth is the load.
