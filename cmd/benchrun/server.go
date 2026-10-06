package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

// serveGUI starts the benchrun web GUI: a profile dropdown + knob inputs + a
// Run button that posts to /api/run and renders the result. Host-only dev tool
// (like cmd/devconsole) — plain embedded HTML + fetch, no build pipeline.
func serveGUI(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, guiHTML)
	})

	// /api/profiles → the saved profiles (populate the dropdown + autofill).
	mux.HandleFunc("/api/profiles", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(allProfiles())
	})

	// /api/sample (POST) → preview the campaigns / request / bids a config
	// generates, so the GUI can show what's actually being load-tested.
	mux.HandleFunc("/api/sample", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var p Profile
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, `{"error":"bad config"}`, http.StatusBadRequest)
			return
		}
		p = clampProfile(p)
		_ = json.NewEncoder(w).Encode(buildSample(p))
	})

	// /api/run (POST) → run once with the posted config, return the Result.
	// Serialized: a run saturates the cores, so a second concurrent run would
	// corrupt both — return 429 if one is already in flight.
	var runMu sync.Mutex
	running := false
	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST only"}`, http.StatusMethodNotAllowed)
			return
		}
		runMu.Lock()
		if running {
			runMu.Unlock()
			http.Error(w, `{"error":"a run is already in progress"}`, http.StatusTooManyRequests)
			return
		}
		running = true
		runMu.Unlock()
		defer func() { runMu.Lock(); running = false; runMu.Unlock() }()

		var p Profile
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, `{"error":"bad config"}`, http.StatusBadRequest)
			return
		}
		p = clampProfile(p)
		dur, err := time.ParseDuration(p.Duration)
		if err != nil || dur <= 0 || dur > 60*time.Second {
			dur = 3 * time.Second
		}
		_ = json.NewEncoder(w).Encode(execute(p, dur))
	})

	fmt.Printf("benchrun GUI → http://%s  (stop the stack first for clean numbers)\n", addr)
	if cluster := stackReachable(); cluster {
		fmt.Println("WARNING: a cluster looks reachable — numbers will be noisy. Pause Rancher for comparable results.")
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println("benchrun serve:", err)
	}
}

// clampProfile bounds browser-supplied config so a run can't wedge the host,
// and normalises the enum fields. Shared by /api/run and /api/sample.
func clampProfile(p Profile) Profile {
	if p.Campaigns <= 0 || p.Campaigns > 100000 {
		p.Campaigns = 200
	}
	if p.Bids <= 0 || p.Bids > 4096 {
		p.Bids = 25
	}
	if p.Concurrency <= 0 || p.Concurrency > 256 {
		p.Concurrency = defaultConcurrency()
	}
	switch p.Targeting {
	case "none", "broad", "dense", "extreme":
	default:
		p.Targeting = "dense"
	}
	switch p.Channel {
	case "display", "video", "audio", "native", "retail", "dooh":
	default:
		p.Channel = "display"
	}
	if p.IOLatencyMs < 0 || p.IOLatencyMs > 1000 {
		p.IOLatencyMs = 0
	}
	if p.Duration == "" {
		p.Duration = "3s"
	}
	return p
}

// stackReachable is a best-effort "is the local stack up" check for the GUI
// banner (mirrors the bench.sh guard's intent, advisory here not fatal).
func stackReachable() bool {
	return exec.Command("kubectl", "cluster-info").Run() == nil
}

const guiHTML = `<!doctype html><html><head><meta charset="utf-8">
<title>benchrun — auction compute load gen</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  body{font:14px -apple-system,system-ui,sans-serif;background:#0b0f17;color:#e5e7eb;margin:0;padding:24px;max-width:900px}
  h1{font-size:18px;margin:0 0 4px} .sub{color:#94a3b8;font-size:12px;margin-bottom:16px}
  details{background:#111827;border:1px solid #1f2937;border-radius:8px;padding:10px 14px;margin-bottom:16px}
  summary{cursor:pointer;font-weight:600;font-size:13px} details .body{margin-top:10px;font-size:12px;color:#cbd5e1;line-height:1.55}
  details ul{margin:6px 0;padding-left:18px} pre{background:#0b0f17;border:1px solid #1f2937;border-radius:6px;padding:10px;overflow:auto;font-size:11px;color:#9ca3af;white-space:pre}
  .grid{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin-bottom:16px}
  label{display:block;font-size:11px;color:#94a3b8;margin-bottom:4px}
  select,input{width:100%;box-sizing:border-box;background:#1e293b;color:#e5e7eb;border:1px solid #334155;border-radius:6px;padding:7px 8px;font:inherit}
  button{background:#6366f1;color:#fff;border:0;border-radius:6px;padding:10px 20px;font:600 14px inherit;cursor:pointer}
  button.ghost{background:#1e293b;border:1px solid #334155} button:disabled{opacity:.5;cursor:default}
  .out{margin-top:20px;background:#111827;border:1px solid #1f2937;border-radius:8px;padding:16px;display:none}
  .big{font-size:30px;font-weight:700;color:#6ee7b7} .big small{font-size:13px;color:#94a3b8;font-weight:400}
  .row{display:flex;gap:24px;margin-top:10px;flex-wrap:wrap} .row div span{display:block;color:#94a3b8;font-size:11px}
  .row div b{font-size:16px;font-variant-numeric:tabular-nums}
  .note{margin-top:14px;color:#fbbf24;font-size:11px}
  .warn{background:#3f1d1d;color:#fca5a5;padding:8px 12px;border-radius:6px;font-size:12px;margin-bottom:16px;display:none}
  .hint{font-size:10px;color:#64748b;margin-top:2px}
</style></head><body>
<h1>benchrun — auction + bid compute load generator</h1>
<div class="sub">Dial the world, hit Run. Measures the COMPUTE path (no I/O) — see "What's being tested".</div>
<div id="warn" class="warn">⚠ A cluster looks reachable — pause Rancher for comparable numbers.</div>

<details>
  <summary>❓ What's being tested — and what's NOT</summary>
  <div class="body">
    Each <b>cycle</b> = one ad request driven through the platform's pure COMPUTE, with fakes for every store (no cluster):
    <pre>request ─▶ DSP bid loop                         ─▶ bids ─▶ exchange auction ─▶ winner(s)
            (for each of N campaigns:                        (strategy by channel:
             creative-size match + targeting.Evaluate)        single-winner / relevance / …)</pre>
    <ul>
      <li><b>Campaigns</b> — size of the DSP book; the bid loop is O(campaigns), so this is the main cost lever.</li>
      <li><b>Targeting</b> — how many dimensions each campaign gates on (none → extreme): drives targeting.Evaluate cost.</li>
      <li><b>Channel</b> — picks the auction strategy (display=single-winner first-price, retail=relevance-weighted multi-winner, dooh=timeslot…).</li>
      <li><b>Bids</b> — how many eligible bids reach the auction (auction cost scales with this).</li>
      <li><b>Concurrency</b> — parallel workers (cores) hammering the compute.</li>
    </ul>
    <b>NOT tested:</b> the real I/O that bounds the live platform — DSP fan-out over the network, Redis, NATS, Postgres,
    JSON on the wire, GC under concurrent load. So auctions/sec here is the <b>compute ceiling</b> (huge); real sustained
    rps is far lower and I/O-bound — measure that with <code>make loadtest-ramp</code> on the running stack. The
    <b>simulated I/O</b> knob below adds a fixed per-cycle wait to ILLUSTRATE how waiting collapses throughput — a teaching
    aid, not real I/O.
  </div>
</details>

<div class="grid">
  <div><label>Profile</label><select id="profile"></select></div>
  <div><label>Campaigns (book size)</label><input id="campaigns" type="number" min="1"></div>
  <div><label>Bids → auction</label><input id="bids" type="number" min="1"></div>
  <div><label>Concurrency (cores)</label><input id="concurrency" type="number" min="1"></div>
  <div><label>Targeting depth</label><select id="targeting">
    <option value="none">none (match-all)</option><option value="broad">broad (geo+device)</option>
    <option value="dense">dense (+segments+categories)</option><option value="extreme">extreme (all dimensions)</option>
  </select></div>
  <div><label>Channel (→ auction strategy)</label><select id="channel">
    <option value="display">display → single-winner</option><option value="video">video → single-winner</option>
    <option value="audio">audio → single-winner</option><option value="native">native → single-winner</option>
    <option value="retail">retail → relevance-weighted</option><option value="dooh">dooh → timeslot</option>
  </select></div>
  <div><label>Duration</label><input id="duration" placeholder="3s"></div>
  <div><label>Simulated I/O (ms/cycle)</label><input id="io_latency_ms" type="number" min="0" value="0">
    <div class="hint">0 = pure compute · >0 = illustrative only</div></div>
</div>
<div style="display:flex;gap:10px">
  <button id="run" onclick="run()">Run</button>
  <button class="ghost" onclick="sample()">Show example data ▾</button>
</div>

<div id="out" class="out">
  <div class="big"><span id="aps">—</span> <small>auctions/sec</small></div>
  <div class="row">
    <div><span>p50</span><b id="p50">—</b></div>
    <div><span>p95</span><b id="p95">—</b></div>
    <div><span>p99</span><b id="p99">—</b></div>
    <div><span>allocs/cycle</span><b id="alloc">—</b></div>
    <div><span>cycles</span><b id="ops">—</b></div>
  </div>
  <div class="note" id="outnote">Compute ceiling — real sustained rps is I/O-bound (make loadtest-ramp).</div>
</div>

<details id="sampleBox" style="display:none;margin-top:16px">
  <summary>Example data for this config</summary>
  <div class="body">
    <div id="sampleSummary" style="margin-bottom:8px"></div>
    <b>Request signals</b><pre id="sampleReq"></pre>
    <b>Campaigns (first 3 of the book)</b><pre id="sampleCamps"></pre>
    <b>Eligible bids → auction (first 3)</b><pre id="sampleBids"></pre>
  </div>
</details>

<script>
let profiles=[];
function cfg(){return{campaigns:+campaigns.value,bids:+bids.value,targeting:targeting.value,channel:channel.value,concurrency:+concurrency.value,duration:duration.value,io_latency_ms:+io_latency_ms.value};}
async function init(){
  profiles=await (await fetch('/api/profiles')).json();
  const sel=document.getElementById('profile');
  sel.innerHTML=profiles.map((p,i)=>'<option value="'+i+'">'+p.name+' — '+(p.description||'')+'</option>').join('');
  sel.onchange=fill; fill();
}
function fill(){
  const p=profiles[document.getElementById('profile').value]||{};
  campaigns.value=p.campaigns||200; bids.value=p.bids||25;
  targeting.value=p.targeting||'dense'; channel.value=p.channel||'display';
  concurrency.value=p.concurrency||0; duration.value=p.duration||'3s';
  io_latency_ms.value=p.io_latency_ms||0;
}
async function run(){
  const btn=document.getElementById('run'); btn.disabled=true; btn.textContent='Running…';
  try{
    const r=await (await fetch('/api/run',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(cfg())})).json();
    if(r.error){alert(r.error);return;}
    out.style.display='block';
    aps.textContent=r.auctions_per_sec.toLocaleString();
    p50.textContent=r.p50_us+'µs'; p95.textContent=r.p95_us+'µs'; p99.textContent=r.p99_us+'µs';
    alloc.textContent='~'+Math.round(r.allocs_per_cycle); ops.textContent=r.ops.toLocaleString();
    outnote.textContent=(+io_latency_ms.value>0)?'Includes '+io_latency_ms.value+'ms simulated I/O/cycle (illustrative).':'Compute ceiling — real sustained rps is I/O-bound (make loadtest-ramp).';
  }catch(e){alert('run failed: '+e);}
  finally{btn.disabled=false; btn.textContent='Run';}
}
async function sample(){
  const box=document.getElementById('sampleBox'); box.style.display='block'; box.open=true;
  const s=await (await fetch('/api/sample',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(cfg())})).json();
  sampleSummary.textContent=s.eligible_bids_of_book+' of the first '+Math.min(s.book_size,12)+' campaigns match → '+s.auction_request.strategy;
  sampleReq.textContent=JSON.stringify(s.request,null,2);
  sampleCamps.textContent=JSON.stringify(s.campaigns,null,2);
  sampleBids.textContent=JSON.stringify(s.bids,null,2);
}
init();
</script></body></html>`
