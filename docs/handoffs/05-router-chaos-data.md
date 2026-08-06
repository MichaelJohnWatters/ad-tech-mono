# Session: router scenario backlog + chaos-bench + perf data science

## Context
The 5-DSP scenario market (memory: project_auction_speed_push, "5-DSP
scenario market") live-tests the SmartRouter every run: slowpoke (timeout
skip), deadbeat (bid-rate skip), coinflip (never-skip guard), healthy,
internal. It caught 5 real router bugs in its first day. perf-archives/
holds raw 15s Prometheus series per benchmark run (pandas-ready).

## Goals (pick with the user; each is standalone)
A. FLAPPER DSP: time-varying dsp.response_delay (square wave ~5min period,
   wall-clock driven) — auto-exercises skip→probe→rehabilitate BOTH ways
   every run + empirically tunes exchange.routing_recency_window.
B. DEAL-HOLDER DSP: terrible open-market bid rate + holds a seeded PMP
   deal + listed in exchange.routing_never_skip — the ONE router path no
   scenario covers; protects contractual money. Needs seed: deal + config.
C. EDGE-RIDER DSP: delay just under bid_timeout (~420ms) — the legal-but-
   painful bidder; motivates latency-based skip policy (currently latency
   only affects ranking).
D. Low-traffic probe thinning: at ~20rps the rolling window under-samples
   and skips flap (see sweep-1 20rps row) — fix (traffic-scaled min_calls
   or window) + test.
E. CHAOS-BENCH: systematize make test-e2e-chaos the way perfbench did load
   (scripts/perfbench.sh is the template): kill pods mid-run on a schedule,
   assert VERIFY + drain + canary after. The week's incident cascade is
   the motivation (memory lists every failure mode found by accident).
F. DATA SCIENCE: notebook/report over perf-archives/*.jsonl.gz + the
   runs.jsonl ledger — e.g. mem-floor vs phase-p95 correlation, conductor-
   overlap effects, sched-latency vs fanout tail.

## Rules
/perf-loadtest skill = protocol; perfbench for any measured claim; commit
per finding with numbers; update the skill + memory when market composition
or baselines change.
