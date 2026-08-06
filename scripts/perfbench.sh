#!/usr/bin/env bash
# perfbench — dated, comparable performance benchmark runs + raw-data archive.
#
# For each RPS stage, runs the FULL perf protocol (the /perf-loadtest skill,
# automated): make reset → seed the 73-campaign big world → warm every cache →
# loadtest with VERIFY → scrape phases/fill/canary/pressure → append one JSON
# line to docs/perf/runs.jsonl → regenerate docs/perf/RESULTS.md (the dated
# comparison table) → archive the run's RAW Prometheus series (15s resolution,
# every adtech_*/node_*/container_*/go_sched*/jetstream_* series) to
# perf-archives/<stamp>-rps<N>/ for offline data science (gitignored — a few
# MB gzipped per 30m run; load with pandas.read_json(lines=True)).
#
# Usage:
#   make perfbench                                   # one stage: RPS=110 30m
#   RPS=150 DURATION=10m make perfbench
#   RPS_STAGES="20 50 80 100 125 150" make perfbench # load-curve sweep,
#                                                    #   clean world per stage
#   PURGE=1 make perfbench                           # full PVC wipe first
#
# Comparability rules are the protocol's: hands-off host during runs, never
# deploy mid-run, fresh world per stage. A red VERIFY still records
# (verify:"RED") — a benchmark that hides failures is worse than none.
set -euo pipefail
cd "$(dirname "$0")/.."

RPS=${RPS:-110}
RPS_STAGES=${RPS_STAGES:-$RPS}
DURATION=${DURATION:-30m}
PURGE=${PURGE:-0}
NOTE=${NOTE:-}
NS=adtech
LEDGER=docs/perf/runs.jsonl
RESULTS=docs/perf/RESULTS.md

if [ "$PURGE" = "1" ]; then
  make stack-down PURGE=1
  make stack-up
  kubectl -n $NS wait --for=condition=ready pod --all --timeout=240s >/dev/null 2>&1 || true
fi

start_pf() {
  kubectl -n $NS port-forward svc/prometheus 19090:9090 >/dev/null 2>&1 &
  PF=$!
  until curl -s -m 2 http://localhost:19090/-/ready >/dev/null 2>&1; do sleep 0.5; done
}
stop_pf() { kill "${PF:-0}" 2>/dev/null || true; }
trap stop_pf EXIT

run_stage() {
  local rps=$1
  local log=/tmp/perfbench-rps${rps}.log
  echo "== perfbench stage: ${rps}rps for ${DURATION} =="

  echo "-- clean world --"
  make reset >/dev/null 2>&1
  go run ./cmd/seed --profile standard --big-world-advertisers 20 \
    --big-world-publishers 5 --big-world-campaigns-per 3 >/dev/null 2>&1

  echo "-- warm caches --"
  for p in 8081 8082 8084 8085 8088 8089 8090 8098; do
    curl -s -X POST -o /dev/null -m 10 "http://localhost:$p/debug/cache/refresh" || true
  done
  for pod in $(kubectl -n $NS get pods -l app=dsp-internal -o name); do
    kubectl -n $NS exec "${pod#pod/}" -- wget -q -O- --post-data= \
      http://localhost:8082/debug/cache/refresh >/dev/null 2>&1 || true
  done

  echo "-- load (hands off the host) --"
  local start end verify=GREEN
  start=$(date +%s)
  make loadtest RPS="$rps" DURATION="$DURATION" VERIFY=1 >"$log" 2>&1 || verify=RED
  end=$(date +%s)

  echo "-- record + archive --"
  start_pf
  local canary sha stamp
  canary=$(kubectl -n $NS exec postgres-0 -- psql -U adtech -d adtech -tAc \
    "SELECT COALESCE((SELECT balance FROM advertiser_balances b JOIN accounts a ON a.id=b.account_id WHERE a.name ILIKE '%canary%' LIMIT 1), 0);" | tr -d ' ')
  sha=$(git rev-parse --short HEAD)
  git status --porcelain | grep -q . && sha="${sha}+dirty"
  stamp=$(date +%Y%m%d-%H%M)

  START=$start END=$end RPS_STAGE=$rps CANARY=$canary SHA=$sha STAMP=$stamp \
  DURATION=$DURATION NOTE=$NOTE VERIFY_STATUS=$verify LEDGER=$LEDGER RESULTS=$RESULTS \
    python3 scripts/perfbench_record.py "$log"
  stop_pf
}

for rps in $RPS_STAGES; do
  run_stage "$rps"
done
echo "== perfbench done: $(wc -l < $LEDGER) runs in the ledger =="
