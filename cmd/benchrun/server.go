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
		// Clamp to sane bounds so a browser can't wedge the host.
		if p.Campaigns <= 0 || p.Campaigns > 100000 {
			p.Campaigns = 200
		}
		if p.Bids <= 0 || p.Bids > 4096 {
			p.Bids = 25
		}
		if p.Concurrency <= 0 || p.Concurrency > 256 {
			p.Concurrency = 0 // execute() → GOMAXPROCS via the caller default below
		}
		if p.Concurrency == 0 {
			p.Concurrency = defaultConcurrency()
		}
		if p.Targeting != "dense" {
			p.Targeting = "broad"
		}
		if p.Channel != "retail" {
			p.Channel = "display"
		}
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

// stackReachable is a best-effort "is the local stack up" check for the GUI
// banner (mirrors the bench.sh guard's intent, advisory here not fatal).
func stackReachable() bool {
	return exec.Command("kubectl", "cluster-info").Run() == nil
}

const guiHTML = `<!doctype html><html><head><meta charset="utf-8">
<title>benchrun — auction compute load gen</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  body{font:14px -apple-system,system-ui,sans-serif;background:#0b0f17;color:#e5e7eb;margin:0;padding:24px;max-width:860px}
  h1{font-size:18px;margin:0 0 4px} .sub{color:#94a3b8;font-size:12px;margin-bottom:20px}
  .grid{display:grid;grid-template-columns:repeat(3,1fr);gap:12px;margin-bottom:16px}
  label{display:block;font-size:11px;color:#94a3b8;margin-bottom:4px}
  select,input{width:100%;box-sizing:border-box;background:#1e293b;color:#e5e7eb;border:1px solid #334155;border-radius:6px;padding:7px 8px;font:inherit}
  button{background:#6366f1;color:#fff;border:0;border-radius:6px;padding:10px 20px;font:600 14px inherit;cursor:pointer}
  button:disabled{opacity:.5;cursor:default}
  .out{margin-top:20px;background:#111827;border:1px solid #1f2937;border-radius:8px;padding:16px;display:none}
  .big{font-size:30px;font-weight:700;color:#6ee7b7} .big small{font-size:13px;color:#94a3b8;font-weight:400}
  .row{display:flex;gap:24px;margin-top:10px;flex-wrap:wrap} .row div span{display:block;color:#94a3b8;font-size:11px}
  .row div b{font-size:16px;font-variant-numeric:tabular-nums}
  .note{margin-top:14px;color:#fbbf24;font-size:11px}
  .warn{background:#3f1d1d;color:#fca5a5;padding:8px 12px;border-radius:6px;font-size:12px;margin-bottom:16px;display:none}
</style></head><body>
<h1>benchrun — auction + bid compute load generator</h1>
<div class="sub">Dial the world, hit Run. Compute ceiling (no I/O) — not platform rps.</div>
<div id="warn" class="warn">⚠ A cluster looks reachable — pause Rancher for comparable numbers.</div>
<div class="grid">
  <div><label>Profile</label><select id="profile"></select></div>
  <div><label>Campaigns (book size)</label><input id="campaigns" type="number" min="1"></div>
  <div><label>Bids → auction</label><input id="bids" type="number" min="1"></div>
  <div><label>Targeting</label><select id="targeting"><option>broad</option><option>dense</option></select></div>
  <div><label>Channel</label><select id="channel"><option>display</option><option>retail</option></select></div>
  <div><label>Concurrency (cores)</label><input id="concurrency" type="number" min="1"></div>
  <div><label>Duration</label><input id="duration" placeholder="3s"></div>
</div>
<button id="run" onclick="run()">Run</button>
<div id="out" class="out">
  <div class="big"><span id="aps">—</span> <small>auctions/sec</small></div>
  <div class="row">
    <div><span>p50</span><b id="p50">—</b></div>
    <div><span>p95</span><b id="p95">—</b></div>
    <div><span>p99</span><b id="p99">—</b></div>
    <div><span>allocs/cycle</span><b id="alloc">—</b></div>
    <div><span>cycles</span><b id="ops">—</b></div>
  </div>
  <div class="note">Compute ceiling — real sustained rps is I/O-bound (make loadtest-ramp).</div>
</div>
<script>
let profiles=[];
async function init(){
  profiles=await (await fetch('/api/profiles')).json();
  const sel=document.getElementById('profile');
  sel.innerHTML=profiles.map((p,i)=>'<option value="'+i+'">'+p.name+' — '+(p.description||'')+'</option>').join('');
  sel.onchange=fill; fill();
}
function fill(){
  const p=profiles[document.getElementById('profile').value]||{};
  document.getElementById('campaigns').value=p.campaigns||200;
  document.getElementById('bids').value=p.bids||25;
  document.getElementById('targeting').value=p.targeting||'broad';
  document.getElementById('channel').value=p.channel||'display';
  document.getElementById('concurrency').value=p.concurrency||0;
  document.getElementById('duration').value=p.duration||'3s';
}
async function run(){
  const btn=document.getElementById('run'); btn.disabled=true; btn.textContent='Running…';
  const body={campaigns:+campaigns.value,bids:+bids.value,targeting:targeting.value,channel:channel.value,concurrency:+concurrency.value,duration:duration.value};
  try{
    const r=await (await fetch('/api/run',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})).json();
    if(r.error){alert(r.error);return;}
    document.getElementById('out').style.display='block';
    document.getElementById('aps').textContent=r.auctions_per_sec.toLocaleString();
    document.getElementById('p50').textContent=r.p50_us+'µs';
    document.getElementById('p95').textContent=r.p95_us+'µs';
    document.getElementById('p99').textContent=r.p99_us+'µs';
    document.getElementById('alloc').textContent='~'+Math.round(r.allocs_per_cycle);
    document.getElementById('ops').textContent=r.ops.toLocaleString();
  }catch(e){alert('run failed: '+e);}
  finally{btn.disabled=false; btn.textContent='Run';}
}
init();
</script></body></html>`
