# Hot-path micro-benchmarks

The cluster-free sibling of the stack-level perf ledger (`docs/perf/runs.jsonl`
+ `make perfbench`). Those measure the whole system under load and attribute a
regression only as far as "which service phase"; these bench the **pure
compute cores** in isolation, so a regression attributes to a **function +
commit at PR time** — no cluster, Redis, or NATS.

## Run

```sh
make bench        # run + diff vs baseline.txt (benchstat; auto-installed once)
make bench-pin    # re-pin baseline.txt after a known-good change (commit it)
```

## What's covered (the selection rule: hot × pure × isolatable)

| Bench | Service core it guards |
|---|---|
| `BenchmarkExchangeAuction` (`pkg/auction`) | Exchange winner selection over the fan-out bid set, swept by bid count |
| `BenchmarkDSPBidPath` (`cmd/dsp`) | DSP per-campaign bid loop (creative-size match + targeting eval), swept by catalog size — O(campaigns), so this is the iron-rule hot loop |

Add a package to `PKGS` in `scripts/bench.sh` only when it has `Benchmark*`
funcs AND sits on the serving path. Tier-2 candidates (not yet done):
`pkg/bidshading`, `pkg/openrtb` codec, `pkg/adserving` macros/signing,
`pkg/fraud/realtime`.

## Reading it

- Absolute ns/op is **machine-specific** — the signal is the **delta on one
  machine across commits**, not the number. Re-pin (`make bench-pin`) when you
  change machines or VM sizing (same rule as the golden `perfbench` BASELINE).
- `~ (p=…)` = no significant change; a `+N%` with low p is a real regression.
- These are **complementary** to `perfbench`, not a replacement: micro-benches
  guard single-function CPU/allocs; they do NOT catch contention, GC pressure
  under concurrency, or the Redis/NATS tail — the stack harness owns those.
