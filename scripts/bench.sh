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

if [ "${1:-}" = "--pin" ]; then
  echo "[bench] running + pinning baseline → $BASELINE"
  run_bench | tee "$BASELINE"
  echo "[bench] baseline pinned. Commit $BASELINE in this PR."
  exit 0
fi

NEW="$OUTDIR/.last.txt"
echo "[bench] running hot-path benches…"
run_bench | tee "$NEW"

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
