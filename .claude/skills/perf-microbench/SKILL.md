---
name: perf-microbench
description: Run and track hot-path Go micro-benchmarks (cluster-free) to catch per-function CPU/allocation regressions at PR time, and measure the compute-ceiling throughput of the auction + bid path. Use when asked to check if a change slowed the hot path, benchmark a pure function, add a new benchmark, re-pin the baseline, or see "how many auctions/sec the compute can do". The sibling of /perf-loadtest — that one measures the whole stack under load; this one isolates the compute core with no cluster.
---

# Hot-path micro-benchmark protocol

Two perf questions, two tools. **`/perf-loadtest`** measures the whole stack
under real load and attributes a regression only as far as "which SERVICE
phase". **This** benches the pure compute cores in isolation, so a regression
attributes to a **function + commit at PR time** — no cluster, Redis, or NATS.
Use both: micro-benches guard the hot-path math cheaply and often; the stack
harness remains the truth for system behaviour under load.

## The daily question: "did my change slow the hot path down?"

```sh
make bench          # run the hot-path benches + diff vs docs/perf/bench/baseline.txt
```

`make bench` runs every `Benchmark*` in the tracked packages `-count=6
-benchmem`, then prints a `benchstat` table comparing this run to the committed
baseline (benchstat is auto-installed on first run; offline → raw output, no
diff). Read it:

- **`~ (p=…)`** → no significant change. Good.
- **`+N%` with low p** on `sec/op` or `allocs/op` → a real regression; the row
  names the function. Investigate before merging.
- Absolute ns/op is **machine-specific** — the signal is the DELTA on one
  machine across commits, never the raw number.

## When you intend the change (re-pinning the baseline)

After a deliberate, known-good change to hot-path cost (e.g. you optimised it,
or accepted a small cost for a feature), re-pin so future diffs compare to the
new reality:

```sh
make bench-pin      # overwrite docs/perf/bench/baseline.txt
```

Commit `docs/perf/bench/baseline.txt` **in the same PR** as the code change, so
the baseline and the code move together. Re-pin ALSO when you change machines
or VM sizing (the numbers are machine-specific). Same discipline as the golden
`perfbench` BASELINE SHA.

## A/B a specific change by hand

```sh
git stash && make bench-pin && git stash pop   # pin the BEFORE state
make bench                                      # diff your change against it
```

(Then restore the real committed baseline with `git checkout docs/perf/bench/baseline.txt`.)

## Configurable load gen with saved profiles (`benchrun`)

When you want to DIAL the world rather than run the fixed benches — campaign
count, eligible-bid count, targeting density, channel, concurrency, duration —
use the `benchrun` tool. It runs the same compute (DSP bid loop → bids →
auction) in a timed, parallel loop and reports auctions/sec + p50/p95/p99
latency + allocs/cycle.

```sh
make bench-run-list                      # saved profiles (profiles/bench/*.yaml)
make bench-run PROFILE=big-world         # 2000 campaigns, all cores, 5s
make bench-run ARGS='-campaigns 500 -targeting dense -channel retail -concurrency 8 -duration 5s'
make bench-run PROFILE=standard ARGS='-concurrency 4'   # profile + flag override
```

Saved profiles: `small` (sanity), `standard` (200-campaign display), `big-world`
(2000-campaign stress), `retail` (relevance-weighted multi-winner), `broad`
(cheap-match). Add one by dropping a YAML in `profiles/bench/`. Any flag
overrides the profile. Same compute-ceiling caveat as below — not platform rps.

## "How many auctions/sec can the compute do?" (the ceiling, not platform rps)

```sh
go test ./pkg/auction/ ./cmd/dsp/ -run '^$' -bench Throughput -cpu 1,2,4,8 -benchtime=10s
```

The `*Throughput` benches drive the auction / bid loop across all cores with
zero I/O and report `auctions/sec` / `bidreqs/sec`. Crank `-cpu` to watch it
scale on a generous box. **This is the COMPUTE CEILING, not platform
throughput** — it deletes the DSP fan-out / Redis / NATS / JSON-on-the-wire
that are the REAL bottleneck, so the number is orders of magnitude above real
rps (compute ≈ 100k+/sec vs the VM's ~180 rps). Its only honest use: proving
the auction/bid MATH has headroom and will never be the limit. For capacity
planning use `/perf-loadtest` + `docs/perf/runs.jsonl`.

## What's tracked (selection rule: hot × pure × isolatable)

A function earns a benchmark only if it's (1) on the per-request/impression hot
path, (2) pure/isolatable (no DB/network — benchable without a cluster), and
(3) otherwise unattributable (a stack regression there is just "phase p95 up").

| Bench | Service core | Package |
|---|---|---|
| `BenchmarkExchangeAuction` (+`Throughput`, `SecondPrice`) | exchange winner selection over the fan-out bid set | `pkg/auction` |
| `BenchmarkDSPBidPath` (+`Throughput`) | DSP per-campaign bid loop (creative-size match + targeting eval), O(campaigns) | `cmd/dsp` |

Tier-2 candidates (not yet benched): `pkg/bidshading` (`ShadedBid`,
`WinRateCurve`), `pkg/openrtb` encode/decode (the hidden alloc cost on the
path), `pkg/adserving` macros + HMAC signing, `pkg/fraud/realtime` scoring.

## Adding a benchmark

1. Write `func BenchmarkX(b *testing.B)` in `<pkg>/bench_test.go`. Build
   fixtures BEFORE `b.ResetTimer()`; call `b.ReportAllocs()` (or run with
   `-benchmem`). Drive the REAL function (use `pkg/testutil` fakes / in-memory
   stores for anything that would otherwise touch I/O — never a live dep).
2. Sweep the scaling dimension with sub-benchmarks (`b.Run("n=%d")`) when cost
   is O(something) — bid count for the auction, catalog size for the bid loop.
3. For a throughput variant: `b.RunParallel` + `b.ReportMetric(float64(b.N)/
   b.Elapsed().Seconds(), "things/sec")`.
4. Add the package to `PKGS` in `scripts/bench.sh`.
5. `make bench-pin` to seed the baseline for the new rows; commit it.

## Invariants & gotchas

- **Fakes, never live deps.** A bench that dials Postgres/Redis isn't a
  micro-bench and won't run in CI — it's `/perf-loadtest` in disguise. Keep the
  hot-path code pure enough to bench (the iron rule already enforces this on the
  bid loop: no per-call network I/O).
- **0 allocs/op on the bid loop is a feature, not luck** — the iron rule. If a
  change makes `BenchmarkDSPBidPath` start allocating, that's a GC-pressure
  regression on the hot path; treat it as a real finding.
- **Not a replacement for the stack harness.** Micro-benches miss contention,
  GC-under-concurrency, and the Redis/NATS tail. Green micro-benches + a red
  `perfbench` = an I/O/VM problem, not a compute one.
- `docs/perf/bench/.last.txt` (transient run output) is gitignored; only
  `baseline.txt` is committed.

## Pointers

- `scripts/bench.sh` — the runner (PKGS list, benchstat install, pin logic).
- `docs/perf/bench/README.md` — what's covered + how to read it.
- `.claude/skills/perf-loadtest/SKILL.md` — the stack-level sibling (real rps,
  money invariant, the golden baseline).
