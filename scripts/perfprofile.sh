#!/usr/bin/env bash
# perfprofile — capture Go profiles from every pod of every hot-path service
# DURING a load run, so each benchmark ships with its own "where did the time
# go" evidence instead of hand-pulled pprof.
#
# Per pod (in parallel): CPU (CPU_SECONDS), then arm block+mutex, wait the same
# window, capture block/mutex/heap/allocs/goroutine, disarm. Profiles are
# pulled via `kubectl exec … wget` on the pod's own HTTP port (the internal mux
# that carries /debug/pprof — every pod, not just the one the LB picks).
# Then a per-service `go tool pprof -top` summary is written next to them.
#
# Usage:
#   scripts/perfprofile.sh <out_dir>                  # profile now
#   DELAY=300 scripts/perfprofile.sh <out_dir>        # wait 5m first (mid-run)
#   SERVICES="dsp-internal ssp" scripts/perfprofile.sh <out_dir>
#   SUMMARIZE=0 …                       # capture only (use during a load run)
#   SUMMARIZE_ONLY=1 scripts/perfprofile.sh <out_dir>  # summarize a capture
#
# Overhead: CPU profiling ~1-3%; block/mutex armed only for the capture
# window (rates below) and disarmed after — keep captures inside the steady
# state of a run, not across its start/end.
set -uo pipefail

OUT=${1:?usage: perfprofile.sh <out_dir>}
NS=${NS:-adtech}
DELAY=${DELAY:-0}
CPU_SECONDS=${CPU_SECONDS:-30}
BLOCK_RATE=${BLOCK_RATE:-10000}   # ns: sample one blocking event per 10µs blocked
MUTEX_FRACTION=${MUTEX_FRACTION:-5}
# Frames that mark "inside a request" (HTTP + gRPC servers) for block/mutex focus.
REQ_FOCUS=${REQ_FOCUS:-'ServeHTTP|handleStream|processUnaryRPC'}
SERVICES=${SERVICES:-"ssp exchange dsp-internal adserver tracker publisher-adserver gateway"}

port_for() {
  case $1 in
    exchange) echo 8081 ;; dsp-internal|dsp-competitor*) echo 8082 ;;
    tracker) echo 8083 ;; ssp) echo 8084 ;; adserver) echo 8085 ;;
    reporting) echo 8086 ;; publisher-adserver) echo 8088 ;; gateway) echo 8080 ;;
    *) echo "" ;;
  esac
}

pull() { # pod port path outfile
  kubectl -n "$NS" exec "$1" -- wget -q -T $((CPU_SECONDS + 30)) -O- "http://localhost:$2$3" >"$4" 2>/dev/null \
    || { rm -f "$4"; echo "  WARN $1 $3 failed" >&2; }
}

profile_pod() { # svc pod port
  local svc=$1 pod=$2 port=$3 dir="$OUT/$1/$2"
  mkdir -p "$dir"
  pull "$pod" "$port" "/debug/pprof/profile?seconds=$CPU_SECONDS" "$dir/cpu.pb.gz"
  pull "$pod" "$port" "/debug/pprof/rates?block=$BLOCK_RATE&mutex=$MUTEX_FRACTION" /dev/null
  pull "$pod" "$port" "/debug/pprof/block" "$dir/block.base.pb.gz"
  pull "$pod" "$port" "/debug/pprof/mutex" "$dir/mutex.base.pb.gz"
  sleep "$CPU_SECONDS"
  pull "$pod" "$port" "/debug/pprof/block" "$dir/block.pb.gz"
  pull "$pod" "$port" "/debug/pprof/mutex" "$dir/mutex.pb.gz"
  pull "$pod" "$port" "/debug/pprof/rates?block=0&mutex=0" /dev/null
  pull "$pod" "$port" "/debug/pprof/heap" "$dir/heap.pb.gz"
  pull "$pod" "$port" "/debug/pprof/allocs" "$dir/allocs.pb.gz"
  pull "$pod" "$port" "/debug/pprof/goroutine?debug=1" "$dir/goroutine.txt"
}

if [ "${SUMMARIZE_ONLY:-0}" != "1" ]; then
[ "$DELAY" -gt 0 ] && sleep "$DELAY"
mkdir -p "$OUT"
echo "perfprofile: capturing ${CPU_SECONDS}s CPU + block/mutex window → $OUT"
for svc in $SERVICES; do
  port=$(port_for "$svc")
  [ -z "$port" ] && { echo "  skip $svc (no port mapping)"; continue; }
  for pod in $(kubectl -n "$NS" get pods -l app="$svc" --field-selector=status.phase=Running -o name); do
    profile_pod "$svc" "${pod#pod/}" "$port" &
  done
done
wait
fi

summarize() {
# Human-readable summary: per service, merged across its pods. Runs `go tool
# pprof` dozens of times — host CPU the simulator + VM share — so perfbench
# calls it AFTER the load ends (SUMMARIZE=0 during capture). Doing it mid-run
# took fleet sched p99 from ~5ms to ~100ms (2026-10-14 run A).
SUMMARY="$OUT/SUMMARY.txt"
: >"$SUMMARY"
for svc in $SERVICES; do
  [ -d "$OUT/$svc" ] || continue
  {
    echo "################ $svc ($(ls "$OUT/$svc" | wc -l | tr -d ' ') pods)"
    for kind in cpu; do
      files=$(ls "$OUT/$svc"/*/$kind.pb.gz 2>/dev/null)
      [ -n "$files" ] && { echo "== $kind (flat top 25)"; go tool pprof -top -nodecount=25 $files 2>/dev/null | sed -n '1,35p'; }
      [ -n "$files" ] && { echo "== $kind (cum top 25)"; go tool pprof -top -cum -nodecount=25 $files 2>/dev/null | sed -n '6,35p'; }
    done
    for kind in block mutex; do
      for d in "$OUT/$svc"/*/; do
        [ -s "$d/$kind.pb.gz" ] && [ -s "$d/$kind.base.pb.gz" ] || continue
        # Request-path only: background loops (janitors, refreshers, pollers)
        # park in select/chanrecv forever and would swamp the top-N.
        echo "== $kind delta over ${CPU_SECONDS}s, request path only — $(basename "$d") (cum top 20)"
        go tool pprof -top -cum -nodecount=20 -focus="$REQ_FOCUS" -base "$d/$kind.base.pb.gz" "$d/$kind.pb.gz" 2>/dev/null | sed -n '6,30p'
      done
    done
    for d in "$OUT/$svc"/*/; do
      [ -s "$d/goroutine.txt" ] && echo "== goroutines $(basename "$d"): $(head -1 "$d/goroutine.txt")"
    done
    echo
  } >>"$SUMMARY"
done
echo "perfprofile: summary — $SUMMARY"
}

if [ "${SUMMARIZE:-1}" = "1" ] || [ "${SUMMARIZE_ONLY:-0}" = "1" ]; then
  summarize
fi
