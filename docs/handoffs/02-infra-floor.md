# Session: trim the infra CPU floor (the last local perf lever)

## Context
Load-curve sweeps (docs/perf/RESULTS.md) show the stack burns ~8.1 of 10
cores BEFORE meaningful traffic — observability + infra, not the auction.
The latency knee sits at 110-150rps purely because the auction fleet gets
the leftovers. Memory: project_auction_speed_push.md ("THE FLOOR EATS
CORES", per-pod cadvisor panels name the residents: TB 1.7GiB/250m const,
clickhouse, promtail ~200m, prometheus (grew with cadvisor series), loki,
jaeger). Node: 10 vCPU / 20GiB (rdctl).

## Goal
Cut the idle/under-load floor measurably (target: −1.5 to −2 cores at
110rps) without losing the observability that earned its keep this week.
Candidate levers: prometheus scrape intervals + cadvisor keep-list,
promtail throughput, loki retention/limits, jaeger trace sampling under
load (tracing.sample_ratio?), TB --development sizing, a lean
`values-bench.yaml` overlay for benchmark runs specifically.

## Protocol
Measure with `make perfbench` (RPS=110 DURATION=30m) before/after — the
ledger + pinned baseline (docs/perf/BASELINE) auto-compares. The
/perf-loadtest skill is the rulebook. A/B one lever at a time; commit each
with numbers. Success = knee moves right (RPS_STAGES sweep at the end).
