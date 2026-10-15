#!/usr/bin/env bash
# perf-config-batch — config-only A/B batch: a same-session CONTROL + one
# arm per live knob, 110rps/10m each via perfbench (2026-10-15 batch: see
# handoff 08 "Config A/B batch"). Edit the arm list at the bottom.
# Each arm: set ONE live config row -> wait until every pod logged the change
# -> perfbench -> revert (trap reverts too if anything fails).
set -uo pipefail
cd /Users/michaeljohnwatters/repo/ad-tech-mono
NS=adtech

psql() { kubectl -n $NS exec postgres-0 -- psql -U adtech -d adtech -tAc "$1"; }
setcfg() { # pod key jsonvalue
  psql "UPDATE config SET value='$3', updated_at=now() WHERE pod_id='$1' AND key='$2' RETURNING value" >/dev/null
}
waitpods() { # app key expected-substring
  local app=$1 key=$2 want=$3
  for i in $(seq 1 12); do
    local ok=1
    for p in $(kubectl -n $NS get pods -l app=$app -o name); do
      kubectl -n $NS logs "$p" --since=3m 2>/dev/null | grep "config changed" | grep "\"key\":\"$key\"" | grep -q "\"new\":\"$want\"" || ok=0
    done
    [ $ok = 1 ] && { echo "  all $app pods applied $key=$want"; return 0; }
    sleep 10
  done
  echo "  WARN: not every $app pod logged $key=$want"; return 1
}

CUR_POD=""; CUR_KEY=""; CUR_OLD=""
revert() {
  if [ -n "$CUR_KEY" ]; then
    setcfg "$CUR_POD" "$CUR_KEY" "$CUR_OLD"; echo "  reverted $CUR_KEY -> $CUR_OLD"
    CUR_KEY=""
  fi
}
trap revert EXIT INT TERM

run() { # note
  RPS=110 DURATION=10m NOTE="config batch: $1" scripts/perfbench.sh > /tmp/claude-cfgbatch-last.log 2>&1
  grep recorded /tmp/claude-cfgbatch-last.log | cut -c1-120
}

arm() { # label app pod key newjson oldjson waitval
  local label=$1 app=$2 pod=$3 key=$4 new=$5 old=$6 waitval=$7
  echo "== $label: $key = $new"
  CUR_POD=$pod; CUR_KEY=$key; CUR_OLD=$old
  setcfg "$pod" "$key" "$new"
  waitpods "$app" "$key" "$waitval"
  run "$label ($key=$waitval)"
  revert
  waitpods "$app" "$key" "$(echo "$old" | tr -d '"')" || true
}

echo "== CONTROL"; run "CONTROL (all defaults)"
arm "T1 ssp L1 TTL 30s"        ssp          ssp-0          ssp.audience_l1_cache_ttl     '"30s"'  '"3s"' 30s
arm "T2 dsp L1 TTL 10s"        dsp-internal dsp-internal-0 dsp.audience_l1_cache_ttl     '"10s"'  '"3s"' 10s
arm "T3 explore 0.2%"          exchange     exchange-0     exchange.routing_explore_pct  '"0.2"'  '"1"'  0.2
arm "T4 display deadline 300ms" exchange    exchange-0     exchange.bid_timeout_by_channel '"display=300ms"' '""' display=300ms
echo "== batch done"
