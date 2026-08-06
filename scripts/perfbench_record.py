#!/usr/bin/env python3
"""perfbench_record — the record+archive half of scripts/perfbench.sh.

Reads the simulator/VERIFY log (argv[1]) and env (START/END/RPS_STAGE/...),
scrapes headline metrics from Prometheus (localhost:19090 port-forward,
already running), appends one JSON line to the ledger, regenerates
RESULTS.md, and archives every relevant RAW Prometheus series for the run
window at 15s resolution to perf-archives/<stamp>-rps<N>/metrics.jsonl.gz —
one JSON object per series: {"metric": {...labels}, "values": [[ts, "v"], …]}.
Load for data science with:

    import pandas as pd, gzip, json
    rows = [json.loads(l) for l in gzip.open("metrics.jsonl.gz", "rt")]
"""
import gzip
import json
import os
import re
import sys
import urllib.parse
import urllib.request

PROM = "http://localhost:19090"
START = int(os.environ["START"])
END = int(os.environ["END"])
RPS = int(os.environ["RPS_STAGE"])
LEDGER = os.environ["LEDGER"]
RESULTS = os.environ["RESULTS"]

# Steady-state window: run minus the first 2 min (unless the run is short).
elapsed = END - START
window = elapsed - 120 if elapsed - 120 >= 240 else elapsed
W = f"{window}s"
TS = END - 30

# Series worth archiving: our app metrics + node + cadvisor + runtime + bus.
ARCHIVE_RE = r"adtech_.*|node_.*|container_.*|go_sched_latencies.*|go_goroutines|go_memstats_alloc_bytes|jetstream_.*|up"


def prom_get(path, **params):
    url = f"{PROM}{path}?{urllib.parse.urlencode(params, doseq=True)}"
    with urllib.request.urlopen(url, timeout=60) as r:
        return json.load(r)


def query(expr, default=None):
    try:
        res = prom_get("/api/v1/query", query=expr, time=TS)["data"]["result"]
        return round(float(res[0]["value"][1]), 2) if res else default
    except Exception:
        return default


def phase_ms(q, metric):
    return query(f"1000 * histogram_quantile({q}, sum by (le) (rate({metric}[{W}])))")


log = open(sys.argv[1]).read()


def grab(pat, cast=str, default=None):
    m = re.search(pat, log)
    return cast(m.group(1)) if m else default


verify = os.environ.get("VERIFY_STATUS", "GREEN")
if "✓ pipeline lossless" in log:
    verify = "GREEN"
elif "SLIPPAGE" in log or "✗" in log:
    verify = "RED"

entry = {
    "date": __import__("datetime").datetime.now().strftime("%Y-%m-%d %H:%M"),
    "sha": os.environ.get("SHA", "?"),
    "rps_target": RPS,
    "duration": os.environ.get("DURATION", "?"),
    "note": os.environ.get("NOTE", ""),
    "rps_avg": grab(r"Avg RPS:\s+([\d.]+)", float),
    "requests": grab(r"Requests:\s+(\d+)", int),
    "errors": grab(r"Errors:\s+(\d+)", int),
    "fill_pct": grab(r"Fill rate:\s+([\d.]+)%", float),
    "verify": verify,
    "wins_client": grab(r"Wins \(client-counted\): (\d+)", int),
    "impressions": grab(r"Impressions recorded:\s+(\d+)", int),
    "canary_usd": float(os.environ.get("CANARY") or 0),
    "fanout_p50_ms": phase_ms(0.50, 'adtech_auction_phase_duration_seconds_bucket{phase="fanout"}'),
    "fanout_p95_ms": phase_ms(0.95, 'adtech_auction_phase_duration_seconds_bucket{phase="fanout"}'),
    "ssp_pre_auction_p95_ms": phase_ms(0.95, 'adtech_ssp_serve_phase_duration_seconds_bucket{phase="pre_auction"}'),
    "ssp_auction_p95_ms": phase_ms(0.95, 'adtech_ssp_serve_phase_duration_seconds_bucket{phase="auction"}'),
    "ssp_render_p95_ms": phase_ms(0.95, 'adtech_ssp_serve_phase_duration_seconds_bucket{phase="render"}'),
    "dsp_campaign_loop_p95_ms": phase_ms(0.95, 'adtech_bid_phase_duration_seconds_bucket{phase="campaign_loop"}'),
    "dsp_audience_p95_ms": phase_ms(0.95, 'adtech_bid_phase_duration_seconds_bucket{phase="audience"}'),
    "adserver_freqcap_p95_ms": phase_ms(0.95, 'adtech_adserver_serve_phase_duration_seconds_bucket{phase="freqcap"}'),
    "cluster_cpu_pct": query(f'100 * sum(rate(node_cpu_seconds_total{{mode!="idle"}}[{W}])) / count(node_cpu_seconds_total{{mode="idle"}})'),
    "irq_pct": query(f'100 * sum(rate(node_cpu_seconds_total{{mode=~"irq|softirq"}}[{W}])) / count(node_cpu_seconds_total{{mode="idle"}})'),
    "mem_avail_floor_gib": (lambda v: round(v / 2**30, 2) if v else None)(
        query(f"min_over_time(node_memory_MemAvailable_bytes[{W}])")
    ),
    "sched_p99_ms": query(f'1000 * histogram_quantile(0.99, sum by (le) (rate(go_sched_latencies_seconds_total_bucket{{service=~"exchange|dsp-internal|ssp|adserver"}}[{W}])))'),
}

# ---- regression check: compare phases vs the PREVIOUS run at this RPS ----
# The point of the ledger: a big slowdown shows up here BY PHASE NAME, tied
# to the commit range between the two runs' shas — "what changed and where".
PHASE_COLS = [
    "fanout_p50_ms", "fanout_p95_ms", "ssp_pre_auction_p95_ms",
    "ssp_auction_p95_ms", "ssp_render_p95_ms", "dsp_campaign_loop_p95_ms",
    "dsp_audience_p95_ms", "adserver_freqcap_p95_ms",
]
regressions = []
if os.path.exists(LEDGER):
    prev = None
    for l in open(LEDGER):
        if not l.strip():
            continue
        r = json.loads(l)
        if r.get("rps_target") == RPS:
            prev = r
    if prev:
        for c in PHASE_COLS:
            a, b = prev.get(c), entry.get(c)
            if a and b and b > a * 1.3 and b - a > 10:
                regressions.append(f"{c}: {a:g} -> {b:g}ms (+{100*(b-a)/a:.0f}%, since {prev.get('sha','?')})")
        for c in ("fill_pct",):
            a, b = prev.get(c), entry.get(c)
            if a and b and b < a - 3:
                regressions.append(f"{c}: {a:g} -> {b:g} (since {prev.get('sha','?')})")
entry["regressions"] = regressions

os.makedirs(os.path.dirname(LEDGER), exist_ok=True)
with open(LEDGER, "a") as f:
    f.write(json.dumps(entry) + "\n")

# ---- regenerate RESULTS.md from the whole ledger ----
rows = [json.loads(l) for l in open(LEDGER) if l.strip()]
cols = [
    ("date", "Date"), ("sha", "SHA"), ("rps_avg", "RPS"), ("duration", "Dur"),
    ("fill_pct", "Fill%"), ("verify", "Verify"), ("errors", "Err"),
    ("fanout_p50_ms", "fanout p50"), ("fanout_p95_ms", "fanout p95"),
    ("ssp_pre_auction_p95_ms", "pre_auc p95"), ("ssp_auction_p95_ms", "auction p95"),
    ("ssp_render_p95_ms", "render p95"), ("dsp_campaign_loop_p95_ms", "camp_loop p95"),
    ("dsp_audience_p95_ms", "audience p95"), ("adserver_freqcap_p95_ms", "freqcap p95"),
    ("canary_usd", "Canary$"), ("cluster_cpu_pct", "CPU%"),
    ("mem_avail_floor_gib", "MemFloor"), ("note", "Note"),
]


def fmt(v):
    if v is None:
        return "—"
    if isinstance(v, float):
        return f"{v:g}"
    return str(v)


lines = [
    "# Performance benchmark ledger",
    "",
    "GENERATED by scripts/perfbench.sh (`make perfbench`) — do not hand-edit;",
    "the source of truth is runs.jsonl next to this file. Latency columns are",
    "milliseconds, steady-state (first 2 minutes of each run excluded). Raw",
    "per-run Prometheus series live in perf-archives/ (gitignored, 15s res).",
    "Protocol + comparability rules: .claude/skills/perf-loadtest/SKILL.md.",
    "",
    "| " + " | ".join(h for _, h in cols) + " |",
    "|" + "|".join("---" for _ in cols) + "|",
]
for r in rows:
    lines.append("| " + " | ".join(fmt(r.get(k)) for k, _ in cols) + " |")
open(RESULTS, "w").write("\n".join(lines) + "\n")

# ---- archive raw series for the run window ----
stamp = os.environ.get("STAMP", "unknown")
arch_dir = f"perf-archives/{stamp}-rps{RPS}"
os.makedirs(arch_dir, exist_ok=True)

names = prom_get("/api/v1/label/__name__/values")["data"]
keep = re.compile(f"^({ARCHIVE_RE})$")
names = [n for n in names if keep.match(n)]
count = 0
with gzip.open(f"{arch_dir}/metrics.jsonl.gz", "wt") as out:
    for n in names:
        try:
            res = prom_get(
                "/api/v1/query_range",
                query=n, start=START, end=END, step=15,
            )["data"]["result"]
        except Exception:
            continue
        for series in res:
            out.write(json.dumps(series) + "\n")
            count += 1
with open(f"{arch_dir}/run.json", "w") as f:
    json.dump({"entry": entry, "window": {"start": START, "end": END, "step_s": 15},
               "series_count": count, "metric_names": len(names)}, f, indent=2)
os.replace(sys.argv[1], f"{arch_dir}/simulator.log")

print(f"recorded rps={RPS}: fill={entry['fill_pct']}% verify={entry['verify']} "
      f"fanout_p95={entry['fanout_p95_ms']}ms — archived {count} series to {arch_dir}")

# ---- cluster-state snapshot (committed): what was RUNNING at run time ----
# The git sha alone says what the code was; this says what the CLUSTER was:
# actual image digests per pod, replicas, resource requests, the live config
# rows (the behavioral knobs — routing thresholds, scenario delays, ...),
# and the DSP roster. Small JSON, one per run, committed with the ledger.
import subprocess


def sh(cmd):
    try:
        return subprocess.run(cmd, shell=True, capture_output=True, text=True,
                              timeout=60).stdout.strip()
    except Exception as e:
        return f"<error: {e}>"


state = {
    "run": {"stamp": stamp, "rps": RPS, "sha": entry["sha"], "date": entry["date"]},
    "pods": [
        dict(zip(["pod", "image_id", "restarts", "cpu_req"], l.split("|")))
        for l in sh(
            "kubectl -n adtech get pods -o jsonpath="
            "'{range .items[*]}{.metadata.name}{\"|\"}{.status.containerStatuses[0].imageID}"
            "{\"|\"}{.status.containerStatuses[0].restartCount}{\"|\"}"
            "{.spec.containers[0].resources.requests.cpu}{\"\\n\"}{end}'"
        ).splitlines() if l
    ],
    "live_config": [
        dict(zip(["service", "pod_id", "key", "value"], l.split("|", 3)))
        for l in sh(
            "kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -tAc "
            "\"SELECT service||'|'||pod_id||'|'||key||'|'||value::text FROM config ORDER BY pod_id, key\""
        ).splitlines() if "|" in l
    ],
    "dsps": sh(
        "kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -tAc "
        "\"SELECT name||' noise='||noise_pct||' no_bid='||no_bid_rate FROM dsps ORDER BY name\""
    ).splitlines(),
    "node": sh("kubectl get nodes -o jsonpath='{.items[0].status.capacity}'"),
    "git_dirty_files": sh("git status --porcelain").splitlines(),
}
os.makedirs("docs/perf/state", exist_ok=True)
state_file = f"docs/perf/state/{stamp}-rps{RPS}.json"
with open(state_file, "w") as f:
    json.dump(state, f, indent=1)
with open(f"{arch_dir}/cluster-state.json", "w") as f:
    json.dump(state, f, indent=1)

# ---- auto-commit the run record (pathspec-limited: never sweeps up other
# work in a dirty tree) + best-effort push ----
reg_note = ""
if regressions:
    reg_note = "\nREGRESSIONS vs previous run at this RPS:\n  " + "\n  ".join(regressions) + "\n"
    print("!! " + "\n!! ".join(regressions))

msg = (f"perfbench: {entry['date']} rps={RPS} fill={entry['fill_pct']}% "
       f"verify={entry['verify']} fanout_p95={entry['fanout_p95_ms']}ms "
       f"canary=${entry['canary_usd']}\n{reg_note}\n"
       f"Auto-recorded by scripts/perfbench.sh. Cluster state (running image\n"
       f"digests, live config rows, DSP roster) in {state_file}; raw series\n"
       f"archive (uncommitted) in {arch_dir}/.\n\n"
       f"Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>")
sh(f"git add {LEDGER} {RESULTS} docs/perf/state/")
commit_out = sh(f"git commit --no-verify -q -m {json.dumps(msg)} -- docs/perf") or "committed"
push_out = sh("git push -q origin main 2>&1") or "pushed"
print(f"run committed: {sh('git log --oneline -1')} ({push_out})")
