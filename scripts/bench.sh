#!/usr/bin/env bash
# bench.sh — hot-path micro-benchmark tracking, the cluster-free sibling of
# perfbench.sh. perfbench measures the whole stack under load (and attributes a
# regression only as far as "which SERVICE phase"); this benches the pure
# compute cores in isolation, so a regression attributes to a FUNCTION + commit
# at PR time, with no cluster, Redis, or NATS.
#
#   scripts/bench.sh            # run benches, diff vs docs/perf/bench/baseline.txt
#   scripts/bench.sh --pin      # run + overwrite the committed baseline (do this
#                               # deliberately, after a known-good change, and
#                               # commit the new baseline in the same PR)
#
# Baseline numbers are machine-specific — the SIGNAL is the delta on ONE
# machine across commits, not the absolute ns/op. Re-pin when you change
# machines or VM sizing (same rule as the golden perfbench BASELINE).
set -euo pipefail
cd "$(dirname "$0")/.."

# The hot-path packages worth guarding at PR time (pure + per-request). Add a
# package here ONLY when it has Benchmark* funcs and is on the serving path.
PKGS=(./pkg/auction ./cmd/dsp)

OUTDIR=docs/perf/bench
BASELINE=$OUTDIR/baseline.txt
mkdir -p "$OUTDIR"

# Comparability guard: the local stack (Rancher Desktop + the adtech pods) burns
# ~8 of 10 cores at idle and thrashes the VM page cache — a micro-bench run
# alongside it produces noisy, incomparable numbers (the same reason perfbench
# has hands-off-host rules). Abort unless the operator explicitly opts in.
# Override: BENCH_ALLOW_STACK=1 make bench.
guard_stack() {
  [ "${BENCH_ALLOW_STACK:-0}" = "1" ] && return
  local running=""
  if pgrep -qfi "Rancher Desktop" 2>/dev/null; then running="Rancher Desktop app"; fi
  if [ -z "$running" ] && pgrep -qfi "lima|qemu-system|rancher-desktop" 2>/dev/null; then running="Rancher Desktop VM"; fi
  # Cheapest positive confirmation: the cluster answers.
  if [ -z "$running" ] && kubectl cluster-info >/dev/null 2>&1; then running="a reachable k8s cluster"; fi
  if [ -n "$running" ]; then
    echo "[bench] ABORT: $running is up — it competes for CPU/VM and poisons micro-bench numbers." >&2
    echo "[bench] Stop Rancher Desktop first (comparability), or set BENCH_ALLOW_STACK=1 to override." >&2
    exit 2
  fi
}
guard_stack

# -benchtime=2x-ish via count; -count=6 gives benchstat enough samples to judge
# significance. -run=^$ skips normal tests. -benchmem: allocs are the usual
# hot-loop regression.
run_bench() {
  go test "${PKGS[@]}" -run '^$' -bench . -benchmem -count="${BENCH_COUNT:-6}" 2>&1 \
    | grep -vE '^(ok|PASS|goos:|goarch:|pkg:|cpu:|\?)' || true
}

ensure_benchstat() {
  if command -v benchstat >/dev/null 2>&1; then BENCHSTAT=benchstat; return; fi
  local gobin; gobin="$(go env GOPATH)/bin/benchstat"
  if [ -x "$gobin" ]; then BENCHSTAT="$gobin"; return; fi
  echo "[bench] installing benchstat (one-time)…" >&2
  if go install golang.org/x/perf/cmd/benchstat@latest >/dev/null 2>&1 && [ -x "$gobin" ]; then
    BENCHSTAT="$gobin"; return
  fi
  BENCHSTAT=""  # offline / install failed — degrade to raw output, no diff
}

# --profile <BenchRegex> [pkg]: run ONE bench with CPU+mem profiling and print
# the top hotspots (which line burns time / allocates). This is the "now show
# me WHERE" step after make bench flags a regression. Profiles land in
# docs/perf/bench/profiles/ (gitignored) for `go tool pprof` drill-down / flame
# graphs (go tool pprof -http=:0 <profile>).
if [ "${1:-}" = "--profile" ]; then
  re="${2:?usage: bench.sh --profile <BenchRegex> [pkg]}"
  pkg="${3:-./pkg/auction}"
  pdir=$OUTDIR/profiles
  mkdir -p "$pdir"
  cpu="$pdir/cpu.out"; mem="$pdir/mem.out"
  echo "[bench] profiling $re in $pkg (cpu+mem)…"
  go test "$pkg" -run '^$' -bench "$re" -benchmem -benchtime=3s \
    -cpuprofile "$cpu" -memprofile "$mem" -o "$pdir/bench.test" 2>&1 \
    | grep -vE '^(ok|PASS|goos:|goarch:|pkg:|cpu:)' || true
  echo
  echo "[bench] top CPU (cum):"
  go tool pprof -top -cum -nodecount=12 "$pdir/bench.test" "$cpu" 2>/dev/null | sed -n '1,18p' || true
  echo
  echo "[bench] top allocations (alloc_space):"
  go tool pprof -top -sample_index=alloc_space -nodecount=12 "$pdir/bench.test" "$mem" 2>/dev/null | sed -n '1,18p' || true
  echo
  echo "[bench] drill down: go tool pprof -http=:0 $pdir/bench.test $cpu   (or $mem)"
  exit 0
fi

# --max: saturation run — drive the auction + bid compute flat-out across a
# core sweep and report PEAK auctions/sec · bidreqs/sec. The "how many can we
# get through" number for the pure compute. NOT platform rps: this deletes the
# DSP fan-out / Redis / NATS / JSON-on-the-wire that actually bound the live
# system (~180 rps on the VM). For real sustained rps use `make loadtest-ramp`.
if [ "${1:-}" = "--max" ]; then
  bt="${BENCH_MAX_TIME:-3s}"
  cpus="${BENCH_MAX_CPUS:-1,2,4,8}"
  echo "[bench] saturating auction + bid compute (ceiling, NOT platform rps) — cpu=$cpus benchtime=$bt"
  out="$OUTDIR/.max.txt"
  go test ./pkg/auction ./cmd/dsp -run '^$' -bench 'Throughput$' -benchtime="$bt" -cpu "$cpus" 2>&1 \
    | grep -E 'Benchmark.*Throughput' | tee "$out" || true
  echo
  echo "[bench] PEAK compute throughput (higher = more headroom):"
  awk '
    /auctions\/sec/ { split($1,a,"-"); gsub(/[^0-9]/,"",a[2]); c=a[2]?a[2]:1;
      for(i=1;i<=NF;i++) if($(i+1)=="auctions/sec"){v=$i} if(v>ab){ab=v;ac=c} }
    /bidreqs\/sec/  { split($1,a,"-"); gsub(/[^0-9]/,"",a[2]); c=a[2]?a[2]:1;
      for(i=1;i<=NF;i++) if($(i+1)=="bidreqs/sec"){v=$i} if(v>bb){bb=v;bc=c} }
    END {
      if(ab) printf "  auctions/sec : %d  (peak @ %s cores)\n", ab, ac
      if(bb) printf "  bidreqs/sec  : %d  (peak @ %s cores)\n", bb, bc
    }' "$out"
  echo "  (compute ceiling — real sustained rps is I/O-bound; see make loadtest-ramp)"
  exit 0
fi

if [ "${1:-}" = "--pin" ]; then
  echo "[bench] running + pinning baseline → $BASELINE"
  run_bench | tee "$BASELINE"
  echo "[bench] baseline pinned. Commit $BASELINE in this PR."
  exit 0
fi

NEW="$OUTDIR/.last.txt"
echo "[bench] running hot-path benches…"
run_bench | tee "$NEW"

# Append one SHA-stamped record to the committed ledger (the micro-bench
# analogue of perfbench's runs.jsonl): git sha + dirty flag + date + go
# version + per-bench averaged ns/op·B/op·allocs/op. Lets you track a
# function's cost ACROSS commits, not just against the single baseline. Raw
# per-run output is kept under history/ (gitignored) for pprof-less eyeballing.
record_history() {
  local sha dirty; sha="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
  [ -n "$(git status --porcelain 2>/dev/null)" ] && dirty="+dirty" || dirty=""
  local stamp; stamp="$(TZ=UTC date +%Y%m%dT%H%M%SZ)"
  mkdir -p "$OUTDIR/history"
  cp "$NEW" "$OUTDIR/history/${stamp}-${sha}${dirty}.txt"
  NEW="$NEW" SHA="${sha}${dirty}" STAMP="$stamp" GOV="$(go version | awk '{print $3}')" \
    python3 - "$LEDGER" <<'PY'
import json, re, os, sys, collections
ledger = sys.argv[1]
rows = collections.defaultdict(lambda: collections.defaultdict(list))
pat = re.compile(r'^(Benchmark\S+)\s')
for line in open(os.environ["NEW"]):
    m = pat.match(line)
    if not m: continue
    name = m.group(1)
    for val, unit in re.findall(r'([\d.]+)\s+(ns/op|B/op|allocs/op|\S+/sec)', line):
        rows[name][unit].append(float(val))
benches = {n: {u: round(sum(v)/len(v), 2) for u, v in units.items()} for n, units in rows.items()}
rec = {"stamp": os.environ["STAMP"], "sha": os.environ["SHA"],
       "go": os.environ["GOV"], "benches": benches}
with open(ledger, "a") as f:
    f.write(json.dumps(rec) + "\n")
print(f"[bench] recorded {len(benches)} benches @ {os.environ['SHA']} → {ledger}")
PY
}
LEDGER="$OUTDIR/history.jsonl"
record_history || echo "[bench] history record skipped"

if [ ! -f "$BASELINE" ]; then
  echo "[bench] no baseline yet — run 'make bench-pin' to create $BASELINE"
  exit 0
fi

ensure_benchstat
if [ -z "$BENCHSTAT" ]; then
  echo "[bench] benchstat unavailable (offline?) — raw results above; compare against $BASELINE by eye"
  exit 0
fi

echo
echo "[bench] delta vs baseline (benchstat old=baseline new=this run):"
# benchstat flags a statistically significant change; a human/CI reads the
# '~' (no change) vs the +/-% columns. Non-zero exit is NOT a failure here —
# CI can add a threshold gate later; this is the reporting surface.
"$BENCHSTAT" "$BASELINE" "$NEW" || true
