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
make bench-profile BENCH=BenchmarkExchangeAuction/bids=100 PKG=./pkg/auction
                  # CPU+mem profile ONE bench → top hotspots (the "where")
```

**Comparability guard:** `make bench` ABORTS if Rancher Desktop / the local
cluster is up — a running stack burns most of the cores and poisons the
numbers. Stop the stack first, or `BENCH_ALLOW_STACK=1 make bench` to override
(never for a committed baseline). This is why `baseline.txt` must be pinned on
a quiet host.

**History ledger:** every `make bench` appends one SHA-stamped record
(`{stamp, sha, go, benches:{ns/op, B/op, allocs/op}}`) to the committed
`history.jsonl` — track a function's cost across commits, not just vs the
single baseline. Raw per-run output + profiles live under `history/` and
`profiles/` (gitignored).

## Profiles — different ad types / data shapes

Beyond input-size sweeps, the `*Profiles` benches exercise DISTINCT code paths:

| Bench | Profiles | Why they differ |
|---|---|---|
| `BenchmarkExchangeAuctionProfiles` | `display-open` (single-winner first-price), `retail-grid` (relevance-weighted multi-winner) | different auction STRATEGY per channel |
| `BenchmarkDSPBidPathProfiles` | `broad` (geo/device), `dense` (segments+categories+keywords), `video` (non-display path) | targeting-eval cost scales with how much targeting the request carries |

Add a row to profile another shape (video pod, DOOH time-slot, native) — the
auction engine picks its strategy from the request, so a profile is just the
right request + bid fixture. (Code-committed; baseline rows for these land on
the next quiet-host `make bench-pin`.)

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
