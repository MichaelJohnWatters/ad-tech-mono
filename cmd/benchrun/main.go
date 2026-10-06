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
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
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
	if p.Concurrency <= 0 {
		p.Concurrency = runtime.GOMAXPROCS(0)
	}
	dur, err := time.ParseDuration(p.Duration)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchrun: bad duration %q: %v\n", p.Duration, err)
		os.Exit(1)
	}

	fmt.Printf("benchrun: profile=%s campaigns=%d bids=%d targeting=%s channel=%s concurrency=%d duration=%s\n",
		p.Name, p.Campaigns, p.Bids, p.Targeting, p.Channel, p.Concurrency, dur)
	printResult(execute(p, dur))
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
func execute(p Profile, dur time.Duration) Result {
	campaigns := buildCampaigns(p.Campaigns)
	req := buildRequest(p.Targeting, p.Channel)
	engine := auction.NewEngine(clock.Real{})

	var ops int64
	// Per-worker latency samples, bounded so recording never dominates memory
	// or pollutes allocs; beyond the cap a worker keeps counting but stops
	// sampling (the rate stays accurate; the histogram is a large sample).
	const perWorkerCap = 500_000
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
				cycle(engine, campaigns, req, p.Channel, p.Bids)
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

// cycle runs one request through the compute: bid loop → bids → auction.
func cycle(engine *auction.Engine, campaigns []models.Campaign, req targeting.Request, channel string, bidCap int) {
	reqW, reqH := 300, 250
	if channel != "display" {
		reqW, reqH = 0, 0
	}
	bids := make([]auction.Bid, 0, bidCap)
	for j := range campaigns {
		c := &campaigns[j]
		if selectCreative(c, reqW, reqH) == "" {
			continue
		}
		if !targeting.Evaluate(c.Targeting, req).Matched {
			continue
		}
		b := auction.Bid{
			DSPID: "dsp-internal", CampaignID: c.ID, CreativeID: c.CreativeID,
			AdvertiserID: c.AccountID, Price: c.BaseBid,
		}
		if channel == "retail" {
			b.Category = "IAB18-5"
			b.Relevance = 0.3 + c.BaseBid/20.0
		}
		bids = append(bids, b)
		if len(bids) >= bidCap {
			break
		}
	}
	ar := auction.AuctionRequest{Channel: channel, PriceMode: "first_price", FloorPrice: 0.50, TraceID: "benchrun"}
	if channel == "retail" {
		ar.SlotCount = 5
		ar.RetailCategories = []string{"IAB18", "IAB18-5"}
	}
	_, _ = engine.RunAuction(nil, bids, ar)
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
func buildCampaigns(n int) []models.Campaign {
	out := make([]models.Campaign, n)
	geos := []string{"USA", "GBR", "CAN", "AUS"}
	for i := 0; i < n; i++ {
		var segs []string
		switch i % 4 {
		case 0:
			segs = []string{"auto_intenders"}
		case 1:
			segs = []string{"luxury_watch_intent"}
		}
		out[i] = models.Campaign{
			ID:         fmt.Sprintf("li-%d", i),
			AccountID:  fmt.Sprintf("adv-%d", i%7),
			CreativeID: fmt.Sprintf("cr-%d", i),
			Creatives: []models.CampaignCreative{
				{ID: fmt.Sprintf("cr-%d-mpu", i), Format: "display", Width: 300, Height: 250},
				{ID: fmt.Sprintf("cr-%d-lead", i), Format: "display", Width: 728, Height: 90},
			},
			BaseBid: 2.0 + float64(i%40)/10.0,
			Format:  "display",
			Status:  "live",
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{
					Geo: []string{geos[i%len(geos)]}, Device: []string{"mobile", "desktop"}, Segments: segs,
				},
				Exclude: targeting.TargetingSet{Categories: []string{"IAB7"}},
			},
		}
	}
	return out
}

func buildRequest(density, channel string) targeting.Request {
	r := targeting.Request{Geo: "USA", Device: "mobile", Channel: channel}
	if channel != "display" {
		r.Device = "ctv"
	}
	if density == "dense" {
		r.OS = "iOS"
		r.Segments = []string{"sports_fans", "auto_intenders", "in_market_auto"}
		r.Categories = []string{"IAB1", "IAB17", "IAB3"}
		r.Keywords = []string{"ev", "sedan", "lease"}
		r.Domain = "demo-news.example"
	}
	return r
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
