// benchrun is a CONFIGURABLE load generator for the auction + bid COMPUTE
// path. Where `make bench` runs fixed Go micro-benchmarks, this lets you dial
// the world — campaign count, eligible-bid count, targeting density, channel,
// concurrency, duration — from the CLI or a saved YAML profile, and reports
// auctions/sec plus p50/p95/p99 latency.
//
// One "cycle" = one full request through the compute: the DSP bid loop
// evaluates the whole campaign book (creative-size match + targeting), the
// matches become bids, and the exchange auction picks winners. No I/O — this
// is the compute CEILING (see the loud note below), not platform rps.
//
//	go run ./cmd/benchrun -profile big-world
//	go run ./cmd/benchrun -campaigns 500 -bids 50 -targeting dense -concurrency 8 -duration 5s
//	go run ./cmd/benchrun -list
//
// Profiles live in profiles/bench/*.yaml; any flag the user sets overrides the
// profile. Guarded to a quiet host (stop the stack) by the caller — see
// `make bench-run`.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/bidshading"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
)

// Profile is a saved benchrun configuration (profiles/bench/<name>.yaml).
// json tags matter: the web GUI reads these lowercased (the dropdown autofill
// + the /api/run POST body), and Go's marshaler would otherwise emit the
// capitalized field names.
type Profile struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Campaigns   int    `yaml:"campaigns" json:"campaigns"`     // size of the DSP warm-cache book (bid-loop cost)
	Bids        int    `yaml:"bids" json:"bids"`               // cap on eligible bids handed to the auction
	Targeting   string `yaml:"targeting" json:"targeting"`     // broad | dense — how much targeting the request carries
	Channel     string `yaml:"channel" json:"channel"`         // display | retail
	Concurrency int    `yaml:"concurrency" json:"concurrency"` // parallel workers (0 = GOMAXPROCS)
	Duration    string `yaml:"duration" json:"duration"`       // run length, e.g. "3s"
	// IOLatencyMs injects a per-cycle sleep to ILLUSTRATE how I/O latency
	// collapses throughput (0 = pure compute ceiling). It is a teaching knob,
	// NOT real I/O — see the note in the GUI / README. A real DSP fan-out /
	// Redis / NATS round-trip is concurrent and contended; a fixed sleep only
	// shows the first-order "waiting dominates" effect.
	IOLatencyMs int `yaml:"io_latency_ms" json:"io_latency_ms"`

	// --- auction/bid shape knobs ---
	PriceMode    string  `yaml:"price_mode" json:"price_mode"`       // first_price | second_price
	FloorPrice   float64 `yaml:"floor_price" json:"floor_price"`     // pre-auction floor filter (default 0.50)
	Slots        int     `yaml:"slots" json:"slots"`                 // winners to fill (retail/pod multi-winner; default 5)
	MatchRate    int     `yaml:"match_rate" json:"match_rate"`       // % of the book that matches the request (0-100)
	CreativesPer int     `yaml:"creatives_per" json:"creatives_per"` // creative variants per campaign (selectCreative loop)
	SlotW        int     `yaml:"slot_w" json:"slot_w"`               // requested creative width (display; default 300)
	SlotH        int     `yaml:"slot_h" json:"slot_h"`               // requested creative height (display; default 250)
	Separation   bool    `yaml:"separation" json:"separation"`       // apply competitive separation before the auction (opt-in; not in the default engine path)
	Shading      string  `yaml:"shading" json:"shading"`             // disabled | conservative | moderate | aggressive — DSP bid shading

	// --- Tier-3 platform stages (MODELED, toggle on/off to A/B their cost) ---
	// The live platform wraps the auction in stages this pure harness normally
	// skips. These toggles add a MODEL of each so you can test with/without:
	// the CPU-bound stages (deals, identity) do real in-memory work; the
	// network-bound stages (freq cap, budget) add one simulated round-trip of
	// IOLatencyMs each (so set a simulated-I/O >0 to see their cost). This is
	// an approximation — real deps are concurrent + contended; `make
	// loadtest-ramp` is the truth.
	StageDeals    bool `yaml:"stage_deals" json:"stage_deals"`       // deal match/priority before the auction (CPU-real)
	StageIdentity bool `yaml:"stage_identity" json:"stage_identity"` // identity-graph expansion for private segments (CPU-real)
	StageFreqCap  bool `yaml:"stage_freq_cap" json:"stage_freq_cap"` // per-user/household frequency cap (Redis — simulated)
	StageBudget   bool `yaml:"stage_budget" json:"stage_budget"`     // budget/balance gate (Redis — simulated)

	// SerialIO picks how the simulated network stages wait. The live platform
	// fans out CONCURRENTLY and blocks on the SLOWEST dependency, so the
	// faithful model (default, SerialIO=false) overlaps them → one round-trip
	// of IOLatencyMs regardless of how many stages are on. SerialIO=true adds
	// them up (the naive serial worst-case) — useful only to contrast "why
	// async matters".
	SerialIO bool `yaml:"serial_io" json:"serial_io"`

	// --- Hardware: how much of the host the binary is allowed to use ---
	// These bound the Go runtime for the run (restored after), so you can
	// simulate a smaller box without changing the real hardware.
	MaxProcs    int `yaml:"max_procs" json:"max_procs"`         // GOMAXPROCS: CPU cores the scheduler may use (0 = all host cores)
	MemLimitMiB int `yaml:"mem_limit_mib" json:"mem_limit_mib"` // GOMEMLIMIT: soft heap ceiling in MiB (0 = none)
}

const profileDir = "profiles/bench"

func main() {
	var (
		profileName = flag.String("profile", "", "named profile from "+profileDir+"/<name>.yaml")
		list        = flag.Bool("list", false, "list available profiles and exit")
		campaigns   = flag.Int("campaigns", 0, "override: campaign book size")
		bids        = flag.Int("bids", 0, "override: eligible bids into the auction")
		targetingF  = flag.String("targeting", "", "override: broad | dense")
		channel     = flag.String("channel", "", "override: display | retail")
		concurrency = flag.Int("concurrency", 0, "override: parallel workers (default GOMAXPROCS)")
		duration    = flag.String("duration", "", "override: run length, e.g. 5s")
		priceMode   = flag.String("price", "", "override: first_price | second_price")
		floor       = flag.Float64("floor", 0, "override: auction floor price")
		slots       = flag.Int("slots", 0, "override: winners to fill (retail/pod)")
		matchRate   = flag.Int("match", -1, "override: %% of the book that matches (0-100)")
		creatives   = flag.Int("creatives", 0, "override: creative variants per campaign")
		shadingF    = flag.String("shading", "", "override: disabled|conservative|moderate|aggressive")
		separationF = flag.Bool("separation", false, "override: apply competitive separation before the auction")
		serialIOF   = flag.Bool("serial-io", false, "override: add network stages serially (off = async fan-out, wait ≈ slowest)")
		sIO         = flag.Int("io", -1, "override: simulated I/O ms per network stage")
		sweepSpec   = flag.String("sweep", "", "sweep one field over values, e.g. campaigns=100,500,1000 or io=0,1,5")
		stDeals     = flag.Bool("stage-deals", false, "Tier-3 (modeled): deal match before auction")
		stIdentity  = flag.Bool("stage-identity", false, "Tier-3 (modeled): identity-graph expansion")
		stFreqCap   = flag.Bool("stage-freqcap", false, "Tier-3 (modeled): frequency cap (simulated Redis)")
		stBudget    = flag.Bool("stage-budget", false, "Tier-3 (modeled): budget gate (simulated Redis)")
		maxProcs    = flag.Int("maxprocs", 0, "hardware: GOMAXPROCS — CPU cores the run may use (0 = all)")
		memLimit    = flag.Int("memlimit", 0, "hardware: GOMEMLIMIT soft heap ceiling in MiB (0 = none)")
		serve       = flag.Bool("serve", false, "start the web GUI instead of running once")
		addr        = flag.String("addr", "localhost:7777", "web GUI listen address (with -serve)")
	)
	flag.Parse()

	if *list {
		listProfiles()
		return
	}
	if *serve {
		serveGUI(*addr)
		return
	}

	// Defaults (used when neither a profile nor a flag sets a field).
	p := Profile{Name: "default", Campaigns: 200, Bids: 25, Targeting: "dense", Channel: "display", Concurrency: 0, Duration: "3s"}
	if *profileName != "" {
		loaded, err := loadProfile(*profileName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "benchrun: %v\n", err)
			os.Exit(1)
		}
		p = loaded
	}
	// Flag overrides win over the profile.
	if *priceMode != "" {
		p.PriceMode = *priceMode
	}
	if *floor > 0 {
		p.FloorPrice = *floor
	}
	if *slots > 0 {
		p.Slots = *slots
	}
	if *matchRate >= 0 {
		p.MatchRate = *matchRate
	}
	if *creatives > 0 {
		p.CreativesPer = *creatives
	}
	if *shadingF != "" {
		p.Shading = *shadingF
	}
	if *separationF {
		p.Separation = true
	}
	if *serialIOF {
		p.SerialIO = true
	}
	if *sIO >= 0 {
		p.IOLatencyMs = *sIO
	}
	if *stDeals {
		p.StageDeals = true
	}
	if *stIdentity {
		p.StageIdentity = true
	}
	if *stFreqCap {
		p.StageFreqCap = true
	}
	if *stBudget {
		p.StageBudget = true
	}
	if *campaigns > 0 {
		p.Campaigns = *campaigns
	}
	if *bids > 0 {
		p.Bids = *bids
	}
	if *targetingF != "" {
		p.Targeting = *targetingF
	}
	if *channel != "" {
		p.Channel = *channel
	}
	if *concurrency > 0 {
		p.Concurrency = *concurrency
	}
	if *duration != "" {
		p.Duration = *duration
	}
	if *maxProcs > 0 {
		p.MaxProcs = *maxProcs
	}
	if *memLimit > 0 {
		p.MemLimitMiB = *memLimit
	}
	p = normalize(p)
	dur, _ := time.ParseDuration(p.Duration)

	if *sweepSpec != "" {
		runSweep(p, *sweepSpec, dur)
		return
	}
	fmt.Printf("benchrun: profile=%s campaigns=%d bids=%d targeting=%s channel=%s concurrency=%d duration=%s\n",
		p.Name, p.Campaigns, p.Bids, p.Targeting, p.Channel, p.Concurrency, dur)
	printResult(execute(p, dur))
}

// normalize fills sane defaults for any unset field + bounds the enums, so a
// bare profile or a flag-only invocation always runs something sensible.
// Shared by the CLI and the web clampProfile.
func normalize(p Profile) Profile {
	if p.Campaigns <= 0 || p.Campaigns > 1_000_000 {
		p.Campaigns = 200
	}
	if p.Bids <= 0 || p.Bids > 4096 {
		p.Bids = 25
	}
	// 0 → GOMAXPROCS. High concurrency is legitimate for "lots of demand"
	// (workers mostly parked on simulated I/O overlap their waits — that's the
	// async-fan-out point), so allow up to 50k goroutines; only a wild value
	// falls back to GOMAXPROCS.
	if p.Concurrency <= 0 || p.Concurrency > 50_000 {
		p.Concurrency = runtime.GOMAXPROCS(0)
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
	if p.Duration == "" {
		p.Duration = "3s"
	}
	if _, err := time.ParseDuration(p.Duration); err != nil {
		p.Duration = "3s"
	}
	if p.PriceMode != "second_price" {
		p.PriceMode = "first_price"
	}
	if p.FloorPrice <= 0 {
		p.FloorPrice = 0.50
	}
	if p.Slots <= 0 {
		if p.Channel == "retail" {
			p.Slots = 5
		} else {
			p.Slots = 1
		}
	}
	if p.MatchRate <= 0 || p.MatchRate > 100 {
		p.MatchRate = 100 // 0/unset → full match (the cap governs bid count)
	}
	if p.CreativesPer <= 0 {
		p.CreativesPer = 2
	}
	if p.SlotW <= 0 {
		p.SlotW = 300
	}
	if p.SlotH <= 0 {
		p.SlotH = 250
	}
	switch p.Shading {
	case "conservative", "moderate", "aggressive", "disabled":
	default:
		p.Shading = "disabled"
	}
	if p.IOLatencyMs < 0 || p.IOLatencyMs > 1000 {
		p.IOLatencyMs = 0
	}
	// Hardware bounds: 0 = "use the host default". Cap cores at the host count
	// (asking for more than exist is meaningless) and keep the mem limit sane.
	if p.MaxProcs < 0 || p.MaxProcs > runtime.NumCPU() {
		p.MaxProcs = 0
	}
	if p.MemLimitMiB < 0 || p.MemLimitMiB > 1_000_000 {
		p.MemLimitMiB = 0
	}
	return p
}

// SweepPoint is one step of a sweep: the swept value and its measured Result.
type SweepPoint struct {
	Value  float64 `json:"value"`
	Result Result  `json:"result"`
}

// sweepFields lists which dimensions a sweep can vary.
var sweepFields = []string{"campaigns", "bids", "concurrency", "io", "floor", "slots", "match", "creatives"}

func applySweep(p Profile, field string, v float64) Profile {
	switch field {
	case "campaigns":
		p.Campaigns = int(v)
	case "bids":
		p.Bids = int(v)
	case "concurrency":
		p.Concurrency = int(v)
	case "io":
		p.IOLatencyMs = int(v)
	case "floor":
		p.FloorPrice = v
	case "slots":
		p.Slots = int(v)
	case "match":
		p.MatchRate = int(v)
	case "creatives":
		p.CreativesPer = int(v)
	}
	return normalize(p)
}

// sweep runs the base config once per value of `field`, returning a point each.
func sweep(base Profile, field string, values []float64, dur time.Duration) []SweepPoint {
	pts := make([]SweepPoint, 0, len(values))
	for _, v := range values {
		pts = append(pts, SweepPoint{Value: v, Result: execute(applySweep(base, field, v), dur)})
	}
	return pts
}

// runSweep is the CLI face: parse "field=v1,v2,…", sweep, print a table.
func runSweep(base Profile, spec string, dur time.Duration) {
	field, values, err := parseSweep(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchrun: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("benchrun sweep: %s over %v  (channel=%s targeting=%s duration=%s)\n", field, values, base.Channel, base.Targeting, dur)
	fmt.Printf("  %-12s %14s %10s %10s\n", field, "auctions/sec", "p50", "p99")
	for _, pt := range sweep(base, field, values, dur) {
		fmt.Printf("  %-12s %14s %10s %10s\n", trimNum(pt.Value), commas(pt.Result.AuctionsPerSec), pt.Result.P50, pt.Result.P99)
	}
}

func parseSweep(spec string) (string, []float64, error) {
	i := strings.IndexByte(spec, '=')
	if i < 0 {
		return "", nil, fmt.Errorf("bad -sweep %q (want field=v1,v2,…; fields: %v)", spec, sweepFields)
	}
	field := strings.TrimSpace(spec[:i])
	if !contains(sweepFields, field) {
		return "", nil, fmt.Errorf("unknown sweep field %q (fields: %v)", field, sweepFields)
	}
	var vals []float64
	for _, s := range strings.Split(spec[i+1:], ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return "", nil, fmt.Errorf("bad sweep value %q", s)
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return "", nil, fmt.Errorf("-sweep %q has no values", spec)
	}
	if len(vals) > 24 {
		return "", nil, fmt.Errorf("-sweep has %d values (max 24)", len(vals))
	}
	return field, vals, nil
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func trimNum(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// Result is one benchrun outcome — shared by the CLI and the web GUI.
type Result struct {
	AuctionsPerSec int64         `json:"auctions_per_sec"`
	Ops            int64         `json:"ops"`
	Duration       string        `json:"duration"`
	P50            time.Duration `json:"-"`
	P95            time.Duration `json:"-"`
	P99            time.Duration `json:"-"`
	P50us          float64       `json:"p50_us"`
	P95us          float64       `json:"p95_us"`
	P99us          float64       `json:"p99_us"`
	AllocsPerCycle float64       `json:"allocs_per_cycle"`
}

// execute runs the compute loop for dur and returns the measured Result
// (no printing — the CLI formats it, the web handler JSON-encodes it).
// runCtx is the resolved per-run state the hot loop reads — built once from a
// Profile so the worker goroutines allocate nothing per cycle.
type runCtx struct {
	engine     *auction.Engine
	campaigns  []models.Campaign
	req        targeting.Request
	auctionReq auction.AuctionRequest
	channel    string
	bidCap     int
	slotW      int
	slotH      int
	shading    string
	curve      bidshading.Curve
	floor      float64
	separation bool
	// Tier-3 modeled stages.
	stageDeals    bool
	stageIdentity bool
	deals         []synthDeal
	graph         map[string][]string
	ioLat         time.Duration // one simulated network round-trip
	netStages     int           // enabled Redis-bound stages (freq cap + budget)
	serialIO      bool          // true = add stage waits serially; false (default) = concurrent fan-out (wait ≈ slowest)
}

func newRunCtx(p Profile) *runCtx {
	rc := &runCtx{
		engine:     auction.NewEngine(clock.Real{}),
		campaigns:  buildCampaigns(p.Campaigns, p.Targeting, p.MatchRate, p.CreativesPer),
		req:        buildRequest(p.Targeting, p.Channel),
		auctionReq: auctionReqFor(p.Channel, p.PriceMode, p.FloorPrice, p.Slots),
		channel:    p.Channel,
		bidCap:     p.Bids,
		slotW:      p.SlotW,
		slotH:      p.SlotH,
		shading:    p.Shading,
		floor:      p.FloorPrice,
		separation: p.Separation,
		stageDeals: p.StageDeals, stageIdentity: p.StageIdentity,
		ioLat:    time.Duration(p.IOLatencyMs) * time.Millisecond,
		serialIO: p.SerialIO,
	}
	if rc.shading != "" && rc.shading != "disabled" {
		rc.curve = seededCurve() // a warm win-rate curve so ShadedBid does real work
	}
	if rc.stageDeals {
		rc.deals = synthDeals(32)
	}
	if rc.stageIdentity {
		rc.graph = synthGraph(2000)
	}
	if p.StageFreqCap {
		rc.netStages++
	}
	if p.StageBudget {
		rc.netStages++
	}
	return rc
}

func execute(p Profile, dur time.Duration) Result {
	// Apply the hardware bounds for the duration of this run, then restore —
	// runs are serialized, so these process-global knobs are safe to toggle.
	if p.MaxProcs > 0 {
		prev := runtime.GOMAXPROCS(p.MaxProcs)
		defer runtime.GOMAXPROCS(prev)
	}
	if p.MemLimitMiB > 0 {
		prev := debug.SetMemoryLimit(int64(p.MemLimitMiB) * 1024 * 1024)
		defer debug.SetMemoryLimit(prev)
	}

	rc := newRunCtx(p)

	var ops int64
	// Per-worker latency samples, bounded so recording never dominates memory
	// or pollutes allocs; beyond the cap a worker keeps counting but stops
	// sampling (the rate stays accurate; the histogram is a large sample).
	// Bound TOTAL latency samples (~1M ≈ 8MB), not per-worker — else high
	// concurrency preallocates gigabytes of buffers and the tool GC-thrashes
	// itself into reporting garbage (the "500 workers collapsed" bug).
	perWorkerCap := 1_000_000 / p.Concurrency
	if perWorkerCap < 1000 {
		perWorkerCap = 1000
	}
	lat := make([][]time.Duration, p.Concurrency)

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < p.Concurrency; w++ {
		w := w
		lat[w] = make([]time.Duration, 0, perWorkerCap)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				start := time.Now()
				cycle(rc)
				atomic.AddInt64(&ops, 1)
				if len(lat[w]) < perWorkerCap {
					lat[w] = append(lat[w], time.Since(start))
				}
			}
		}()
	}
	wg.Wait()
	runtime.ReadMemStats(&memAfter)

	all := make([]time.Duration, 0, ops)
	for _, w := range lat {
		all = append(all, w...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(q float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		return all[int(q*float64(len(all)-1))]
	}
	secs := dur.Seconds()
	p50, p95, p99 := pct(0.50), pct(0.95), pct(0.99)
	return Result{
		AuctionsPerSec: int64(float64(ops) / secs),
		Ops:            ops,
		Duration:       dur.String(),
		P50:            p50, P95: p95, P99: p99,
		P50us: float64(p50.Microseconds()), P95us: float64(p95.Microseconds()), P99us: float64(p99.Microseconds()),
		AllocsPerCycle: float64(memAfter.Mallocs-memBefore.Mallocs) / float64(max64(ops, 1)),
	}
}

// cycle runs one request through the compute, including any enabled Tier-3
// stages (modeled): [deals] → [identity] → DSP bid loop → [separation] →
// auction → [simulated Redis stages].
func cycle(rc *runCtx) {
	if rc.stageDeals {
		matchDeals(rc.deals, rc.auctionReq.PlacementID) // CPU-real deal match
	}
	if rc.stageIdentity {
		resolveIdentity(rc.graph, "pid-benchrun") // CPU-real graph walk
	}
	bids := buildBids(rc)
	if rc.separation {
		bids = auction.FilterBidsWithSeparation(bids, auction.NewSeparationContext())
	}
	_, _ = rc.engine.RunAuction(nil, bids, rc.auctionReq)
	// Simulated network stages. The real auction fires these CONCURRENTLY and
	// blocks on the slowest (async fan-out) — the default models that: spawn a
	// goroutine per stage, wait for all, so the cost ≈ one round-trip no matter
	// how many stages are on. serialIO=true adds them up (naive worst-case) to
	// contrast why async matters.
	if rc.netStages > 0 && rc.ioLat > 0 {
		if rc.serialIO {
			time.Sleep(time.Duration(rc.netStages) * rc.ioLat)
		} else {
			var wg sync.WaitGroup
			wg.Add(rc.netStages)
			for s := 0; s < rc.netStages; s++ {
				go func() { defer wg.Done(); time.Sleep(rc.ioLat) }()
			}
			wg.Wait()
		}
	}
}

// buildBids runs the DSP bid loop (creative-size match + targeting + optional
// shading) over the book and returns the eligible bids, capped at bidCap.
// Shared by cycle() and the GUI's /api/sample so the preview matches the run.
func buildBids(rc *runCtx) []auction.Bid {
	reqW, reqH := rc.slotW, rc.slotH
	if rc.channel != "display" {
		reqW, reqH = 0, 0
	}
	shade := rc.shading != "" && rc.shading != "disabled"
	bids := make([]auction.Bid, 0, rc.bidCap)
	for j := range rc.campaigns {
		c := &rc.campaigns[j]
		if selectCreative(c, reqW, reqH) == "" {
			continue
		}
		if !targeting.Evaluate(c.Targeting, rc.req).Matched {
			continue
		}
		price := c.BaseBid
		if shade {
			price = bidshading.ShadedBid(rc.curve, rc.shading, c.BaseBid, rc.floor)
		}
		b := auction.Bid{
			DSPID: "dsp-internal", CampaignID: c.ID, CreativeID: c.CreativeID,
			AdvertiserID: c.AccountID, Price: price,
		}
		if rc.channel == "retail" {
			b.Category = "IAB18-5"
			b.Relevance = 0.3 + c.BaseBid/20.0
		}
		bids = append(bids, b)
		if len(bids) >= rc.bidCap {
			break
		}
	}
	return bids
}

func auctionReqFor(channel, priceMode string, floor float64, slots int) auction.AuctionRequest {
	if priceMode != "second_price" {
		priceMode = "first_price"
	}
	if floor <= 0 {
		floor = 0.50
	}
	ar := auction.AuctionRequest{Channel: channel, PriceMode: priceMode, FloorPrice: floor, TraceID: "benchrun"}
	if channel == "retail" {
		if slots <= 0 {
			slots = 5
		}
		ar.SlotCount = slots
		ar.RetailCategories = []string{"IAB18", "IAB18-5"}
	} else if slots > 1 {
		ar.SlotCount = slots
	}
	return ar
}

// --- modeled Tier-3 stages (CPU-real parts) ---

type synthDeal struct {
	ID          string
	PlacementID string
	Floor       float64
	Priority    int
}

func synthDeals(n int) []synthDeal {
	d := make([]synthDeal, n)
	for i := range d {
		d[i] = synthDeal{ID: fmt.Sprintf("deal-%d", i), PlacementID: fmt.Sprintf("plc-%d", i%8), Floor: 0.5 + float64(i%10)/10.0, Priority: i % 4}
	}
	return d
}

// matchDeals models the exchange's pre-auction deal match/priority scan: walk
// the deal set for the placement and pick the highest-priority match. Real
// CPU over a deal list (the pure part of pkg/deals).
func matchDeals(deals []synthDeal, placement string) string {
	best, bestPri := "", -1
	for i := range deals {
		if deals[i].PlacementID == placement && deals[i].Priority > bestPri {
			best, bestPri = deals[i].ID, deals[i].Priority
		}
	}
	return best
}

func synthGraph(n int) map[string][]string {
	g := make(map[string][]string, n)
	g["pid-benchrun"] = []string{"he-1", "hh-1", "uid2-1"}
	g["he-1"] = []string{"pid-benchrun", "dev-1"}
	g["dev-1"] = []string{"he-1", "pid-alt"}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("pid-%d", i)
		g[k] = []string{fmt.Sprintf("he-%d", i%500), fmt.Sprintf("hh-%d", i%300)}
	}
	return g
}

// resolveIdentity models the DSP's identity-graph expansion: a bounded BFS
// from the user key (the pure part — the live version also hits Redis for the
// segment sets, which the freq-cap/budget simulated stages stand in for).
func resolveIdentity(graph map[string][]string, userKey string) int {
	seen := map[string]bool{userKey: true}
	frontier := []string{userKey}
	for depth := 0; depth < 3 && len(frontier) > 0; depth++ {
		var next []string
		for _, id := range frontier {
			for _, nb := range graph[id] {
				if !seen[nb] {
					seen[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	return len(seen)
}

// seededCurve warms a win-rate curve so ShadedBid does real work (an empty
// curve short-circuits). Models a placement with observed clearing prices.
func seededCurve() bidshading.Curve {
	t := bidshading.NewTracker()
	for i := 0; i < 60; i++ {
		t.RecordWin("plc-bench", "adv", 2.0+float64(i%30)/10.0, 1.5+float64(i%20)/10.0)
	}
	return t.WinRateCurve("plc-bench")
}

func printResult(r Result) {
	fmt.Println("────────────────────────────────────────────────")
	fmt.Printf("  auctions/sec : %s  (%d cycles in %s)\n", commas(r.AuctionsPerSec), r.Ops, r.Duration)
	fmt.Printf("  latency p50  : %s\n", r.P50)
	fmt.Printf("  latency p95  : %s\n", r.P95)
	fmt.Printf("  latency p99  : %s\n", r.P99)
	fmt.Printf("  allocs/cycle : ~%.0f\n", r.AllocsPerCycle)
	fmt.Println("────────────────────────────────────────────────")
	fmt.Println("  NOTE: compute ceiling (no I/O). Real sustained rps is")
	fmt.Println("  I/O-bound — use `make loadtest-ramp` with the stack up.")
}

func loadProfile(name string) (Profile, error) {
	path := filepath.Join(profileDir, name+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("read profile %q: %w", name, err)
	}
	var p Profile
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return Profile{}, fmt.Errorf("parse profile %q: %w", name, err)
	}
	if p.Name == "" {
		p.Name = name
	}
	return p, nil
}

// profileNameRe bounds a saveable profile name to a safe, path-traversal-proof
// filename slug (lowercase letters, digits, dashes).
var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// saveProfile writes a config to profiles/bench/<name>.yaml (the GUI "Save as"
// face). Validates the name as a safe slug, refuses to clobber an existing
// profile unless overwrite is set, and marshals via the Profile yaml tags so
// the saved file reads exactly like the hand-written presets.
func saveProfile(p Profile, overwrite bool) error {
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if !profileNameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q — use lowercase letters, digits and dashes (max 41 chars)", p.Name)
	}
	p.Name = name
	path := filepath.Join(profileDir, name+".yaml")
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("profile %q already exists", name)
		}
	}
	out, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", profileDir, err)
	}
	return os.WriteFile(path, out, 0o644)
}

// allProfiles returns every saved profile (for the GUI dropdown), name-sorted.
func allProfiles() []Profile {
	entries, _ := filepath.Glob(filepath.Join(profileDir, "*.yaml"))
	sort.Strings(entries)
	var out []Profile
	for _, e := range entries {
		name := filepath.Base(e)
		name = name[:len(name)-len(".yaml")]
		if p, err := loadProfile(name); err == nil {
			if p.Concurrency <= 0 {
				p.Concurrency = runtime.GOMAXPROCS(0)
			}
			out = append(out, p)
		}
	}
	return out
}

func listProfiles() {
	entries, _ := filepath.Glob(filepath.Join(profileDir, "*.yaml"))
	if len(entries) == 0 {
		fmt.Println("no profiles in", profileDir)
		return
	}
	fmt.Println("available benchrun profiles:")
	for _, e := range entries {
		name := filepath.Base(e)
		name = name[:len(name)-len(".yaml")]
		if p, err := loadProfile(name); err == nil {
			fmt.Printf("  %-14s %s\n", name, p.Description)
		}
	}
}

// buildCampaigns / buildRequest / selectCreative mirror the DSP bid loop's
// inputs (kept local — cmd/dsp's fixture builders live in _test.go). Same
// shape as cmd/dsp/bench_test.go's makeBenchCampaigns.
// buildCampaigns builds a synthetic DSP book.
//   - depth      — how many targeting DIMENSIONS each campaign gates on (the
//     real targeting.Evaluate cost lever): none → broad → dense → extreme.
//   - matchRate  — % of the book that MATCHES the request (deterministic): a
//     matching campaign gets the request's geo + a segment it carries; a
//     non-matching one fails on geo. Controls how many become bids.
//   - creativesPer — creative variants per campaign (the selectCreative loop);
//     the first is always 300x250 so the default slot matches.
var altSizes = [][2]int{{728, 90}, {300, 600}, {160, 600}, {970, 250}, {320, 50}}

func buildCampaigns(n int, depth string, matchRate, creativesPer int) []models.Campaign {
	if creativesPer < 1 {
		creativesPer = 2
	}
	out := make([]models.Campaign, n)
	for i := 0; i < n; i++ {
		match := (i % 100) < matchRate
		geo := "GBR" // non-match: request is USA
		var seg []string
		if match {
			geo = "USA"
			if i%4 != 1 { // most matching campaigns carry a segment the request has
				seg = []string{"auto_intenders"}
			} else {
				seg = []string{"luxury_watch_intent"} // still matches on geo; exercises a segment the request lacks
			}
		}
		inc := targeting.TargetingSet{}
		exc := targeting.TargetingSet{}
		switch depth {
		case "none":
			// match-all — but matchRate still shapes the fixture; with no
			// inclusions every campaign matches regardless of geo.
		case "broad":
			inc.Geo = []string{geo}
			inc.Device = []string{"mobile", "desktop"}
		case "extreme":
			inc.Geo = []string{geo}
			inc.Device = []string{"mobile", "desktop", "ctv"}
			inc.Segments = seg
			inc.Categories = []string{"IAB1", "IAB17"}
			inc.Keywords = []string{"ev", "sedan", "lease", "suv"}
			inc.OS = []string{"iOS", "Android"}
			inc.InventoryType = []string{"site", "app"}
			inc.Domains = []string{"demo-news.example", "sports.example"}
			exc.Categories = []string{"IAB7", "IAB25"}
		default: // "dense"
			inc.Geo = []string{geo}
			inc.Device = []string{"mobile", "desktop"}
			inc.Segments = seg
			inc.Categories = []string{"IAB1", "IAB17"}
			exc.Categories = []string{"IAB7"}
		}
		crs := make([]models.CampaignCreative, 0, creativesPer)
		crs = append(crs, models.CampaignCreative{ID: fmt.Sprintf("cr-%d-mpu", i), Format: "display", Width: 300, Height: 250})
		for k := 1; k < creativesPer; k++ {
			s := altSizes[(k-1)%len(altSizes)]
			crs = append(crs, models.CampaignCreative{ID: fmt.Sprintf("cr-%d-%d", i, k), Format: "display", Width: s[0], Height: s[1]})
		}
		out[i] = models.Campaign{
			ID:         fmt.Sprintf("li-%d", i),
			AccountID:  fmt.Sprintf("adv-%d", i%7),
			CreativeID: crs[0].ID,
			Creatives:  crs,
			BaseBid:    2.0 + float64(i%40)/10.0,
			Format:     "display",
			Status:     "live",
			Targeting:  targeting.Rules{Include: inc, Exclude: exc},
		}
	}
	return out
}

// buildRequest carries the request-side signals matching the depth level, so
// campaigns actually match (and the deeper levels exercise more dimensions).
func buildRequest(depth, channel string) targeting.Request {
	r := targeting.Request{Geo: "USA", Device: "mobile", Channel: channel}
	if channel != "display" && channel != "native" {
		r.Device = "ctv"
	}
	switch depth {
	case "none", "broad":
		// geo/device only
	case "extreme":
		r.OS = "iOS"
		r.Segments = []string{"sports_fans", "auto_intenders", "in_market_auto"}
		r.Categories = []string{"IAB1", "IAB17", "IAB3"}
		r.Keywords = []string{"ev", "sedan", "lease"}
		r.Domain = "demo-news.example"
		r.InventoryType = "site"
	default: // dense
		r.OS = "iOS"
		r.Segments = []string{"sports_fans", "auto_intenders", "in_market_auto"}
		r.Categories = []string{"IAB1", "IAB17", "IAB3"}
	}
	return r
}

// SampleData previews what a given config generates — shown in the GUI so you
// can see the campaigns, request signals, and resulting bids before running.
type SampleData struct {
	Request    targeting.Request `json:"request"`
	AuctionReq struct {
		Channel          string   `json:"channel"`
		Strategy         string   `json:"strategy"`
		SlotCount        int      `json:"slot_count,omitempty"`
		RetailCategories []string `json:"retail_categories,omitempty"`
		PriceMode        string   `json:"price_mode"`
	} `json:"auction_request"`
	Campaigns []sampleCampaign `json:"campaigns"`
	Bids      []sampleBid      `json:"bids"`
	TotalBids int              `json:"eligible_bids_of_book"`
	BookSize  int              `json:"book_size"`
}

type sampleCampaign struct {
	ID                string   `json:"id"`
	Advertiser        string   `json:"advertiser"`
	BaseBid           float64  `json:"base_bid"`
	IncludeGeo        []string `json:"include_geo,omitempty"`
	IncludeDevice     []string `json:"include_device,omitempty"`
	IncludeSegments   []string `json:"include_segments,omitempty"`
	IncludeCategories []string `json:"include_categories,omitempty"`
	ExcludeCategories []string `json:"exclude_categories,omitempty"`
	Creatives         []string `json:"creatives"`
}

type sampleBid struct {
	CampaignID string  `json:"campaign_id"`
	Advertiser string  `json:"advertiser"`
	Price      float64 `json:"price"`
	Category   string  `json:"category,omitempty"`
	Relevance  float64 `json:"relevance,omitempty"`
}

// buildSample produces a small, representative preview for the GUI: the first
// few campaigns, the request, the auction shape, and the first few eligible
// bids — all from the SAME builders a real run uses.
func buildSample(p Profile) SampleData {
	sp := p
	sp.Campaigns = 12 // build a small book for the preview
	if p.Campaigns < 12 {
		sp.Campaigns = p.Campaigns
	}
	rc := newRunCtx(sp)
	campaigns := rc.campaigns
	bids := buildBids(rc)
	ar := rc.auctionReq

	var sd SampleData
	sd.Request = rc.req
	sd.AuctionReq.Channel = ar.Channel
	sd.AuctionReq.Strategy = strategyName(p.Channel)
	sd.AuctionReq.SlotCount = ar.SlotCount
	sd.AuctionReq.RetailCategories = ar.RetailCategories
	sd.AuctionReq.PriceMode = ar.PriceMode
	sd.BookSize = p.Campaigns
	sd.TotalBids = len(bids)
	for i := range campaigns {
		if i >= 3 {
			break
		}
		c := campaigns[i]
		crs := make([]string, 0, len(c.Creatives))
		for _, cr := range c.Creatives {
			crs = append(crs, fmt.Sprintf("%s %dx%d", cr.ID, cr.Width, cr.Height))
		}
		sd.Campaigns = append(sd.Campaigns, sampleCampaign{
			ID: c.ID, Advertiser: c.AccountID, BaseBid: c.BaseBid,
			IncludeGeo: c.Targeting.Include.Geo, IncludeDevice: c.Targeting.Include.Device,
			IncludeSegments: c.Targeting.Include.Segments, IncludeCategories: c.Targeting.Include.Categories,
			ExcludeCategories: c.Targeting.Exclude.Categories, Creatives: crs,
		})
	}
	for i := range bids {
		if i >= 3 {
			break
		}
		b := bids[i]
		sd.Bids = append(sd.Bids, sampleBid{
			CampaignID: b.CampaignID, Advertiser: b.AdvertiserID, Price: b.Price,
			Category: b.Category, Relevance: b.Relevance,
		})
	}
	return sd
}

// strategyName reports which auction strategy a channel selects (for the preview).
func strategyName(channel string) string {
	switch channel {
	case "retail":
		return "relevance_weighted (multi-winner)"
	case "dooh":
		return "timeslot → single_winner"
	default:
		return "single_winner (first-price)"
	}
}

// selectCreative is a local copy of the DSP's size-match rule (cmd/dsp's lives
// in package main, not importable). reqW/H 0 → primary creative (non-display).
func selectCreative(c *models.Campaign, reqW, reqH int) string {
	if reqW == 0 && reqH == 0 {
		return c.CreativeID
	}
	for _, cr := range c.Creatives {
		if cr.Width == reqW && cr.Height == reqH {
			return cr.ID
		}
	}
	return ""
}

func defaultConcurrency() int { return runtime.GOMAXPROCS(0) }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// commas formats n with thousands separators.
func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
