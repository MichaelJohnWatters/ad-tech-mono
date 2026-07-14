#!/usr/bin/env bash
# e2e-preflight: sanity-check the stack before an e2e run.
#
# HISTORY: this script used to pin reporting to the `memory` analytics backend
# (and later a single replica) because the harness's /debug read-backs only
# existed there. That magic is gone — ClickHouse implements
# analytics.DebugReader, the HotColdStore wrapper exposes it (HotStore()), and
# harness.Reset truncates the ClickHouse tables — so e2e now runs against the
# REAL backend the platform ships with (hot ClickHouse + cold DuckDB/Delta).
#
# What remains is a fail-fast check that reporting is actually up and able to
# answer read-backs, so a broken stack fails in seconds instead of timing out
# test by test.
set -euo pipefail

if ! curl -fsS -m 5 -o /dev/null "http://localhost:8086/readyz"; then
  echo "[e2e-preflight] reporting not ready on :8086 — is the tilt stack up?" >&2
  exit 1
fi
status=$(curl -s -m 5 -o /dev/null -w '%{http_code}' \
  "http://localhost:8086/debug/auction_wins?trace_id=00000000000000000000000000000000" || echo 000)
if [ "$status" != "200" ]; then
  echo "[e2e-preflight] reporting /debug read-backs unavailable (status $status)." >&2
  echo "[e2e-preflight] the analytics backend must implement DebugReader (memory or clickhouse)." >&2
  exit 1
fi
echo "[e2e-preflight] reporting ready — read-backs answering on the live analytics backend"
