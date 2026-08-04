#!/usr/bin/env bash
# loadtest-ramp.sh — progressive full-path load test.
#
# Runs the simulator through increasing RPS stages (default: 100 150 250), each
# for STAGE_DURATION (default 5m), with the simulator's own --verify gate after
# every stage (reporting must have recorded impressions == wins — the
# zero-slippage check). A stage that errors, aborts (all-fail fast path), or
# fails verification stops the ramp so you know the highest healthy rate.
#
#   make loadtest-ramp
#   RPS_STAGES="100 200 400" STAGE_DURATION=3m make loadtest-ramp
#
# Prereqs: stack up + seeded (make stack-up && make demo). The simulator itself
# fail-fasts with guidance if the world is empty.
set -uo pipefail

STAGES="${RPS_STAGES:-100 150 250}"
DUR="${STAGE_DURATION:-5m}"

echo "=== Progressive load ramp: stages [${STAGES}] × ${DUR} each ==="
for rps in ${STAGES}; do
  echo
  echo "--- Stage: ${rps} rps for ${DUR} (with post-stage pipeline verify) ---"
  if ! go run ./cmd/simulator run --profile steady --rps "${rps}" --duration "${DUR}" --verify; then
    echo
    echo "RAMP STOPPED at ${rps} rps: the stage failed (errors, empty-world abort,"
    echo "or the pipeline verify found slippage). The previous stage is the highest"
    echo "known-healthy rate."
    exit 1
  fi
done
echo
echo "=== Ramp complete: all stages healthy ==="
