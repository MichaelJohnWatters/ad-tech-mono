# Session: the kernel-side CPU floor (~4.5 cores at 110rps)

## Context
The 2026-08-07 infra-floor session (02, done) fixed the pod-level waste and
halved tails twice over. What remains at 110rps: pods ~4.8 cores, kernel
~4.5 (system 2.4 / irq 1.65 / softirq 0.8) — now the dominant cost and the
ceiling between here and ~200rps locally. Memory: project_infra_floor_trim
(floor anatomy + what's ruled out), project_stack_doctor (VZ degradation
signature — read BEFORE trusting any weird bench numbers).

## What's already measured (don't redo)
- Packet volume is NOT the story: ~2.1k TCP segs/s, ~1k pkts/s per iface at
  110rps. Kernel cost is interrupt/exit-bound, not throughput-bound.
- 36k interrupts/s CONSTANT (idle == load): CAL (function-call IPIs)
  ~18.5k/s + LOC (local timer) ~17.8k/s. Each is VM-exit overhead under
  Virtualization.framework. irq mode has a ~1.0-core floor at total idle.
- Context switches 67k/s idle → 94k/s at 110rps (~850/request marginal).
- TCP ActiveOpens 73/s at IDLE (probes/scrapes make fresh connections) —
  135/s under load. Modest, but free to trim.
- GOMAXPROCS=limit pinning: PROVEN BAD (fanout p95 2x) — do not retry.

## Candidate levers (in rough order)
1. **Who generates the CAL IPIs?** Unknown. Needs in-VM tracing
   (`perf`/bpftrace if installable in the RD VM, or /proc/softirqs +
   /proc/interrupts per-CPU deltas while suspending suspects one at a time:
   kube-proxy? k3s? TB io_uring? the Go fleet collectively?).
2. **Wakeup accounting per process**: /proc/<pid>/status voluntary_ctxt
   deltas across the fleet → rank the 67k/s idle switches; attack the top
   (likely candidates: NATS heartbeats, redis, TB polling, Go netpollers).
3. **Probe/scrape connection churn**: kubelet probes + prometheus scrapes
   open fresh TCP each time (73/s idle). Probe periods are chart-tunable;
   prometheus keep-alive is config.
4. **Fewer vCPUs** (10→8): counter-intuitive — fewer idle vCPUs = fewer
   timer targets/IPIs/exits, and the auction never gets >5.5 pod-cores
   anyway. Cheap A/B: rdctl set --cpus 8 + the standard 110/10m bench.
   Risk: less burst headroom at 150+.
5. **VM tuning**: RD/lima VZ options are limited, but check virtio-net
   queue count and whether the VM kernel runs NOHZ_FULL/HZ_100.

## Protocol
Same as ever: /perf-loadtest skill, `make perfbench` RPS=110 DURATION=10m
per A/B (user prefers 10m), baseline = docs/perf/BASELINE (5c00c38-era,
fanout p95 ~46-48ms). NEW rules from 2026-08-07: jetstream pending ≈ 0
BEFORE each run; after any VM restart run ~5min `make traffic` (router
rehab) before benching; if tails look 3-8x off with identical config,
suspect VZ degradation (stack-doctor memory) — rdctl restart, don't debug
the app.
