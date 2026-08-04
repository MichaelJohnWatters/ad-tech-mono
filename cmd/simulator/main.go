// cmd/simulator generates production-like ad traffic for testing.
//
// Unlike a thin banner-only load generator, every request is built from a
// Persona (pkg/simulator/request): a coherent user with geo, device, identity
// type, audience segments, and a privacy regime, combined with a channel
// (display/video/audio/native). The resulting OpenRTB request populates every
// field the exchange, DSP, targeting, privacy, identity, fraud, and deals code
// actually reads — so simulated traffic drives the same paths as production.
//
// Usage:
//
//	simulator run --profile steady --duration 5m
//	simulator run --profile burst --channel video
//	simulator run --profile trickle --persona eu-consented-mobile
//	simulator single --persona us-ccpa-optout --channel native
//	simulator personas
//	simulator profiles
//	simulator check
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/request"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

var log = logger.New("simulator")

// beaconLost64 counts wins whose impression beacon never delivered after 3
// attempts — the reconciliation term for client-side beacon loss.
var beaconLost64 int64

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runSimulation()
	case "single":
		runSingle()
	case "profiles":
		listProfiles()
	case "personas":
		listPersonas()
	case "check":
		checkServices()
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Ad Tech Simulator

Usage:
  simulator run       Run a simulation profile
  simulator single    Fire a single production-like bid request
  simulator profiles  List available load profiles
  simulator personas  List available personas (user + consent + identity)
  simulator check     Check if services are reachable

By default the simulator mirrors the web /dev/publisher-simulator: each channel
goes through the same first-party endpoint the web tab uses (SSP serve for
display, publisher-adserver for video/native/audio) and fires the server's
signed beacons. --direct is a raw load mode that POSTs OpenRTB to the exchange.

Run flags:
  --profile <name>    Profile: trickle, steady, burst (default: trickle)
  --duration <dur>    Duration: 30s, 5m, 1h (default: 1m)
  --requests <n>      Stop after N requests (0 = use duration)
  --rps <n>           Aggregate rate cap (0 = spam: fire as fast as the backend takes it)
  --concurrency <n>   Parallel in-flight requests / worker pool size (default: 2×rps, floor 64)
  --conv-rate <f>     Override conversion rate (fraction of clicks that convert)
  --ssp-url <u>       SSP URL (default: http://localhost:8084)
  --pubad-url <u>     Publisher ad server URL (default: http://localhost:8088)
  --exchange-url <u>  Exchange URL for --direct mode (default: http://localhost:8081)
  --tracker-url <u>   Tracker URL for --direct mode (default: http://localhost:8083)
  --persona <name>    Force a single persona (see 'simulator personas')
  --channel <ch>      Force a channel: display, video, audio, native
  --pod <n>           Request a CTV ad pod of n ads (video channel)
  --direct            Raw load mode: POST OpenRTB to the exchange directly
  --geo <geo>         Override geo on every request (ISO alpha-3)
  --device <type>     Override device: mobile, desktop, tablet, ctv
  --verify            After the run, read counts back from reporting and assert
                      the pipeline recorded them (impressions == wins); exits
                      non-zero on mismatch. Use against a quiescent stack.
  --reporting-url <u> Reporting URL for --verify (default: http://localhost:8086)

Single flags:
  --persona <name>    Persona (default: us-personalised-mobile)
  --channel <ch>      Channel: display, video, audio, native (default: display)
  --pod <n>           Request a CTV ad pod of n ads (video channel)
  --direct            Raw load mode: POST OpenRTB to the exchange directly
  --geo <geo>         Override persona geo
  --device <type>     Override persona device`)
}

// channelWeight is one entry in a profile's channel mix.
type channelWeight struct {
	Ch request.Channel
	W  int
}

// profile is a load shape: a request rate, a pool of personas to sample from,
// and a channel mix. Personas carry geo/device/identity/consent so a profile's
// realism comes from its pool, not from flat geo/device lists.
type profile struct {
	Name       string
	RPS        int
	Personas   []request.Persona
	Channels   []channelWeight
	FloorPrice float64
	ClickRate  float64
	ViewPct    int
	// ConvRate is P(conversion | click) — the sim plays the advertiser's site
	// pixel for this fraction of the clicks it fires (real conversions happen
	// off-platform, so there's no human to generate them; we synthesise them).
	ConvRate float64
}

func personaPool(names ...string) []request.Persona {
	out := make([]request.Persona, 0, len(names))
	for _, n := range names {
		if p, ok := request.PersonaByName(n); ok {
			out = append(out, p)
		}
	}
	return out
}

var profiles = map[string]profile{
	// trickle: a gentle, mostly-US display stream for smoke tests.
	"trickle": {
		Name: "trickle", RPS: 1,
		Personas:   personaPool("us-personalised-mobile", "uk-consented-desktop"),
		Channels:   []channelWeight{{request.Display, 1}},
		FloorPrice: 0.50, ClickRate: 0.05, ViewPct: 80, ConvRate: 0.15,
	},
	// steady: a broad, realistic open-exchange blend across regions, consent
	// regimes, and every serving format (display/native/video/audio).
	"steady": {
		Name: "steady", RPS: 10,
		Personas:   request.Personas, // full registry, weighted
		Channels:   []channelWeight{{request.Display, 55}, {request.Native, 20}, {request.Video, 15}, {request.Audio, 10}},
		FloorPrice: 1.00, ClickRate: 0.02, ViewPct: 70, ConvRate: 0.10,
	},
	// burst: high volume across every channel including audio + CTV personas.
	"burst": {
		Name: "burst", RPS: 100,
		Personas:   request.Personas,
		Channels:   []channelWeight{{request.Display, 45}, {request.Video, 25}, {request.Audio, 15}, {request.Native, 15}},
		FloorPrice: 0.50, ClickRate: 0.01, ViewPct: 60, ConvRate: 0.08,
	},
}

func runSimulation() {
	profileName := getFlag("--profile", "trickle")
	durationSet := getFlag("--duration", "") != ""
	duration := parseDuration(getFlag("--duration", "1m"))
	maxRequests := parseInt(getFlag("--requests", "0"))
	// --requests is the stop condition when set (per the flag docs) — don't let
	// the DEFAULT 1m duration truncate a large bounded run (e.g. --requests 1M
	// would otherwise cap at ~1 minute). An explicitly-passed --duration still
	// applies as a combined cap.
	if maxRequests > 0 && !durationSet {
		duration = 100 * 365 * 24 * time.Hour
	}
	exchangeURL := getFlag("--exchange-url", routes.DefaultExchangeURL)
	trackerURL := getFlag("--tracker-url", routes.DefaultTrackerURL)
	eps := endpoints{
		SSP:   getFlag("--ssp-url", routes.DefaultSSPURL),
		PubAd: getFlag("--pubad-url", routes.DefaultPublisherAdServerURL),
	}
	// Default: mirror the web simulator — go through the SSP / publisher-adserver
	// per channel and fire the server-returned signed beacons. --direct is the
	// raw load mode: build OpenRTB and POST straight to the exchange (external-
	// SSP style), self-firing beacons.
	directMode := hasFlag("--direct")
	podSize := parseInt(getFlag("--pod", "1"))

	p, ok := profiles[profileName]
	if !ok {
		fmt.Printf("Unknown profile: %s\n", profileName)
		listProfiles()
		os.Exit(1)
	}

	if rps := getFlag("--rps", ""); rps != "" {
		p.RPS = parseInt(rps)
	}
	if cr := getFlag("--conv-rate", ""); cr != "" {
		if v, err := strconv.ParseFloat(cr, 64); err == nil {
			p.ConvRate = v
		}
	}
	// --persona narrows the pool to one persona; --channel forces one channel.
	forcedPersona := getFlag("--persona", "")
	if forcedPersona != "" {
		pool := personaPool(forcedPersona)
		if len(pool) == 0 {
			fmt.Printf("Unknown persona: %s\n", forcedPersona)
			listPersonas()
			os.Exit(1)
		}
		p.Personas = pool
	}
	forcedChannel := getFlag("--channel", "")
	if forcedChannel != "" {
		p.Channels = []channelWeight{{request.Channel(forcedChannel), 1}}
	}
	geoOverride := getFlag("--geo", "")
	deviceOverride := getFlag("--device", "")

	// Load the seeded placement pool so requests spread across every publisher.
	initInventory(getFlag("--publishers-dir", "profiles/publishers"), log)

	log.Info("simulation starting",
		"profile", p.Name, "rps", p.RPS, "duration", duration,
		"max_requests", maxRequests, "personas", len(p.Personas),
		"exchange", exchangeURL, "tracker", trackerURL,
	)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	// A high-idle-conn transport so concurrent workers reuse connections instead
	// of exhausting ephemeral ports / re-dialing on every request.
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        4096,
			MaxIdleConnsPerHost: 4096,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Worker pool: --concurrency workers each fire runOne independently, so
	// requests fly in PARALLEL. A sequential loop caps at 1/round-trip-latency
	// (~25/sec locally) no matter what --rps says — this removes that wall.
	// --rps still caps the AGGREGATE rate via a shared token ticker; --rps 0
	// removes the cap → spam as fast as the backend can take it.
	// Default scales with the requested rate: a full win iteration (serve +
	// beacons) runs ~0.5-1s locally, so a fixed 64-worker pool silently capped
	// real throughput at ~100/s however high --rps was set. 2×rps keeps the
	// requested rate reachable up to ~2s per iteration; 64 stays the floor.
	concurrency := parseInt(getFlag("--concurrency", "0"))
	if concurrency < 1 {
		concurrency = 2 * p.RPS
		if concurrency < 64 {
			concurrency = 64
		}
	}
	var tokens <-chan time.Time
	if p.RPS > 0 {
		ticker := time.NewTicker(time.Second / time.Duration(p.RPS))
		defer ticker.Stop()
		tokens = ticker.C
	}
	log.Info("worker pool", "concurrency", concurrency, "rps_cap", p.RPS, "spam", p.RPS == 0)

	stop := make(chan struct{})
	var stopOnce sync.Once
	halt := func() { stopOnce.Do(func() { close(stop) }) }
	time.AfterFunc(duration, halt)

	var dispatched, completed, wins64, errors64 int64
	var abortedAllErrors atomic.Bool
	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			wrng := rand.New(rand.NewSource(seed))
			for {
				select {
				case <-stop:
					return
				default:
				}
				if maxRequests > 0 && atomic.AddInt64(&dispatched, 1) > int64(maxRequests) {
					return
				}
				if tokens != nil { // aggregate rate cap (skipped in spam mode)
					select {
					case <-stop:
						return
					case <-tokens:
					}
				}
				traceID, traceparent := tracing.NewClientTraceparent()
				persona := request.Pick(wrng, p.Personas)
				applyOverrides(&persona, geoOverride, deviceOverride)
				channel := pickChannel(wrng, p.Channels)

				served, err := runOne(client, eps, exchangeURL, trackerURL, persona, channel, podSize, p, wrng, traceID, traceparent, directMode)
				atomic.AddInt64(&completed, 1)
				if err != nil {
					e := atomic.AddInt64(&errors64, 1)
					// First few ERRORS (not requests) — a mid-run failure
					// burst was undiagnosable when only requests 1-3 logged.
					if e <= 5 || e%1000 == 0 {
						log.Error("request failed", "error", err, "trace_id", traceID, "errors_so_far", e)
					}
					// Fail fast when EVERY early request errors: that's a dead
					// stack or an unseeded/reset world (e.g. placement 404s),
					// and burning the whole run at full rate proves nothing.
					if e >= 100 && e == atomic.LoadInt64(&completed) {
						abortedAllErrors.Store(true)
						halt()
						return
					}
					continue
				}
				if served {
					atomic.AddInt64(&wins64, 1)
				}
			}
		}(rng.Int63() + int64(w)*7919)
	}

	// Progress logger: throughput once a second from the atomic counters.
	runDone := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-runDone:
				return
			case <-t.C:
				c := atomic.LoadInt64(&completed)
				w := atomic.LoadInt64(&wins64)
				el := time.Since(start)
				log.Info("progress",
					"sent", c, "wins", w, "errors", atomic.LoadInt64(&errors64),
					"win_rate", fmt.Sprintf("%.1f%%", pct(int(w), int(c))),
					"rps", fmt.Sprintf("%.0f", float64(c)/el.Seconds()),
					"elapsed", el.Round(time.Second),
				)
			}
		}
	}()

	wg.Wait()
	close(runDone)

	sent := int(atomic.LoadInt64(&completed))
	wins := int(atomic.LoadInt64(&wins64))
	errors := int(atomic.LoadInt64(&errors64))

	if abortedAllErrors.Load() {
		printResults(sent, wins, errors, time.Since(start))
		fmt.Println("\nABORTED: the first requests ALL failed — the stack is down or the world is")
		fmt.Println("unseeded (a fresh install or a post-e2e reset leaves no placements: serve 404s).")
		fmt.Println("Fix with `make stack-doctor` (stack health) or `make reset` / `make demo` (seed),")
		fmt.Println("then rerun.")
		os.Exit(1)
	}
	printResults(sent, wins, errors, time.Since(start))
	// --verify: read the counts back out of reporting and assert the pipeline
	// recorded what we fired (impressions == wins). Exit non-zero on mismatch so
	// it's usable as a CI/scripts gate, not just a human-readable report.
	if hasFlag("--verify") {
		if !verifyPipeline(getFlag("--reporting-url", routes.DefaultReportingURL), start, sent, wins, errors) {
			os.Exit(1)
		}
	}
}

func runSingle() {
	personaName := getFlag("--persona", "us-personalised-mobile")
	channel := request.Channel(getFlag("--channel", "display"))
	exchangeURL := getFlag("--exchange-url", routes.DefaultExchangeURL)
	trackerURL := getFlag("--tracker-url", routes.DefaultTrackerURL)
	eps := endpoints{
		SSP:   getFlag("--ssp-url", routes.DefaultSSPURL),
		PubAd: getFlag("--pubad-url", routes.DefaultPublisherAdServerURL),
	}
	directMode := hasFlag("--direct")
	podSize := parseInt(getFlag("--pod", "1"))

	persona, ok := request.PersonaByName(personaName)
	if !ok {
		fmt.Printf("Unknown persona: %s\n", personaName)
		listPersonas()
		os.Exit(1)
	}
	applyOverrides(&persona, getFlag("--geo", ""), getFlag("--device", ""))

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	traceID, traceparent := tracing.NewClientTraceparent()
	client := &http.Client{Timeout: 5 * time.Second}

	mode := "web-mirror (SSP/pubad)"
	if directMode {
		mode = "direct (exchange)"
	}
	fmt.Printf("Trace ID: %s\n", traceID)
	fmt.Printf("Persona:  %s (%s, %s, id=%s, consent=%s)\n",
		persona.Name, persona.Geo, persona.Device, persona.Identity, persona.Regime)
	fmt.Printf("Channel:  %s | Mode: %s\n\n", channel, mode)

	served, err := runOne(client, eps, exchangeURL, trackerURL, persona, channel, podSize, profiles["trickle"], rng, traceID, traceparent, directMode)
	if err != nil {
		fmt.Printf("Request: FAILED (%v)\n", err)
		return
	}
	if served {
		fmt.Println("Ad:      SERVED (beacons fired)")
	} else {
		fmt.Println("Ad:      NO FILL")
	}
	fmt.Printf("\nDone. Trace ID: %s\n", traceID)
}

// runOne executes a single request. Default mirrors the web simulator (through
// the SSP / publisher-adserver, firing the server's signed beacons); --direct
// posts OpenRTB straight to the exchange and self-fires beacons (raw load mode).
func runOne(client *http.Client, eps endpoints, exchangeURL, trackerURL string, persona request.Persona, ch request.Channel, pod int, p profile, rng *rand.Rand, traceID, traceparent string, direct bool) (bool, error) {
	// Pick a seeded placement for this channel so traffic spreads across every
	// publisher. Falls back to the built-in simulator placement when the
	// inventory has none for the channel (native/audio) or wasn't loaded.
	pl, haveSeeded := invPick(ch, rng)
	if !direct {
		key := placementKeyFor(ch)
		if haveSeeded {
			key = pl.Key
		}
		return serveMirror(client, eps, persona, ch, pod, p, rng, traceparent, key)
	}
	placement := simPlacement(p.FloorPrice)
	if haveSeeded {
		placement = request.Placement{
			Domain:      pl.Domain,
			Name:        pl.Name,
			Page:        "https://" + pl.Domain + "/article",
			PublisherID: pl.PublisherID,
			TagID:       pl.TagID,
			Categories:  pl.Categories,
			BidFloor:    pl.Floor,
		}
	}
	bidReq := request.Build(request.Input{TraceID: traceID, Channel: ch, Persona: persona, Placement: placement, Rand: rng})
	winner, err := sendAuction(client, exchangeURL, bidReq, traceparent)
	if err != nil {
		return false, err
	}
	if winner == nil {
		return false, nil
	}
	if !fireEvents(client, trackerURL, traceID, traceparent, ch, winner, placement, p, rng) {
		atomic.AddInt64(&beaconLost64, 1)
	}
	return true, nil
}

func hasFlag(name string) bool {
	for _, a := range os.Args {
		if a == name {
			return true
		}
	}
	return false
}

// simPlacement returns the simulator publisher's placement with the real seeded
// UUIDs (idgen-derived), so Site.Publisher.ID + Imp.TagID match production and
// deal eligibility can actually resolve.
func simPlacement(floor float64) request.Placement {
	return request.Placement{
		Domain:      "publisher-simulator.local",
		Name:        "Publisher Simulator",
		Page:        "http://localhost:8080/dev/publisher-simulator",
		PublisherID: idgen.Derive("publisher", "pub-simulator"),
		TagID:       idgen.Derive("placement", "pl-sim-mpu"),
		Categories:  []string{"IAB12"},
		Keywords:    "news,sports,finance,tech",
		BidFloor:    floor,
	}
}

func applyOverrides(p *request.Persona, geo, device string) {
	if geo != "" {
		p.Geo = geo
	}
	if device != "" {
		p.Device = device
	}
}

func pickChannel(rng *rand.Rand, mix []channelWeight) request.Channel {
	total := 0
	for _, c := range mix {
		total += c.W
	}
	if total == 0 {
		return request.Display
	}
	n := rng.Intn(total)
	for _, c := range mix {
		n -= c.W
		if n < 0 {
			return c.Ch
		}
	}
	return mix[len(mix)-1].Ch
}

func listProfiles() {
	fmt.Println("Available profiles:")
	fmt.Println()
	for _, name := range []string{"trickle", "steady", "burst"} {
		p := profiles[name]
		chans := make([]string, len(p.Channels))
		for i, c := range p.Channels {
			chans[i] = fmt.Sprintf("%s:%d", c.Ch, c.W)
		}
		fmt.Printf("  %-10s  %3d rps  personas: %-3d  channels: %s\n",
			name, p.RPS, len(p.Personas), strings.Join(chans, " "))
	}
}

func listPersonas() {
	fmt.Println("Available personas:")
	fmt.Println()
	fmt.Printf("  %-26s %-5s %-8s %-14s %s\n", "NAME", "GEO", "DEVICE", "IDENTITY", "CONSENT REGIME")
	for _, p := range request.Personas {
		fmt.Printf("  %-26s %-5s %-8s %-14s %s\n", p.Name, p.Geo, p.Device, p.Identity, p.Regime)
	}
}

func checkServices() {
	exchangeURL := getFlag("--exchange-url", routes.DefaultExchangeURL)
	trackerURL := getFlag("--tracker-url", routes.DefaultTrackerURL)
	dspURL := routes.DefaultDSPURL

	services := map[string]string{
		"Exchange": exchangeURL + routes.Healthz,
		"DSP":      dspURL + routes.Healthz,
		"Tracker":  trackerURL + routes.Healthz,
	}

	allOK := true
	for name, url := range services {
		resp, err := http.Get(url)
		if err != nil || resp.StatusCode != 200 {
			fmt.Printf("  %-10s  UNREACHABLE  %s\n", name, url)
			allOK = false
		} else {
			fmt.Printf("  %-10s  OK           %s\n", name, url)
			resp.Body.Close()
		}
	}

	if !allOK {
		fmt.Println("\nSome services are not reachable. Start them first (tilt up).")
		os.Exit(1)
	}
	fmt.Println("\nAll services reachable. Ready to simulate.")
}

// sendAuction POSTs the bid request to the exchange and returns the winning bid
// (nil on no-bid) so the caller can fire correctly-attributed, signed beacons
// using the real winner's campaign/creative/price — not placeholder values.
func sendAuction(client *http.Client, exchangeURL string, bidReq openrtb.BidRequest, traceparent string) (*openrtb.BidObj, error) {
	body, _ := json.Marshal(bidReq)
	req, err := http.NewRequest("POST", exchangeURL+routes.OpenRTBAuction, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", constants.ContentTypeJSON)
	req.Header.Set("traceparent", traceparent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var bidResp openrtb.BidResponse
	json.NewDecoder(resp.Body).Decode(&bidResp)
	if bidResp.NoBid || len(bidResp.SeatBid) == 0 || len(bidResp.SeatBid[0].Bid) == 0 {
		return nil, nil
	}
	return &bidResp.SeatBid[0].Bid[0], nil
}

// fireEvents fires the post-win beacon sequence appropriate to the channel:
// an impression for every format, then video/audio quartile events for
// instream, and a stochastic viewability + click for display/native.
//
// Every beacon URL is built via pkg/adserving with the REAL winner's
// campaign/creative/price and the same HMAC signing the platform's ad server
// uses — so the tracker records correctly-attributed events that pass
// tracker.signature_validation, identical to a real render. The only synthetic
// part is the browser-shaped User-Agent (a headless client) and that quartiles
// are fired in sequence rather than from real playback timing.
// fireEvents fires the win's tracker beacons; returns whether the IMPRESSION
// beacon was delivered (the money event — everything else is best-effort,
// mirroring real browser beacon semantics).
func fireEvents(client *http.Client, trackerURL, traceID, traceparent string, ch request.Channel, winner *openrtb.BidObj, pl request.Placement, p profile, rng *rand.Rand) bool {
	mc := adserving.MacroContext{
		AuctionID:    traceID, // the auction trace — ties beacons to the auction
		AuctionPrice: winner.Price,
		Currency:     "USD",
		CampaignID:   winner.CID,
		CreativeID:   winner.CrID,
		PlacementID:  pl.TagID,
		PublisherID:  pl.PublisherID,
		BidModel:     firstNonEmpty(winner.BidModel, "cpm"),
		TrackerURL:   trackerURL,
		URLTTL:       time.Hour,
	}

	// The impression beacon is the MONEY event — deliver it reliably (3
	// attempts) and report failure so reconciliation can account for it.
	// Fire-and-forget silently lost ~1.1% of impressions at 109rps
	// (2026-07-19 clean-slate run): wins said 169,536, the tracker only
	// ever RECEIVED 167,691 — the platform recorded every one it got.
	impDelivered := false
	for attempt := 0; attempt < 3 && !impDelivered; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(100*attempt) * time.Millisecond)
		}
		impDelivered = fireGet(client, adserving.BuildImpressionURL(mc), traceparent)
	}

	switch ch {
	case request.Video:
		for _, ev := range []string{"start", "firstQuartile", "midpoint", "thirdQuartile", "complete"} {
			fireGet(client, adserving.BuildVideoEventURL(mc, ev), traceparent)
		}
		if rng.Float64() < p.ClickRate {
			fireGet(client, adserving.BuildClickURL(mc), traceparent)
		}
	case request.Audio:
		for _, ev := range []string{"start", "firstQuartile", "midpoint", "thirdQuartile", "complete"} {
			fireGet(client, adserving.BuildAudioEventURL(mc, ev), traceparent)
		}
	default: // display, native
		if rng.Intn(100) < p.ViewPct {
			fireGet(client, adserving.BuildViewabilityURL(mc), traceparent)
		}
		if rng.Float64() < p.ClickRate {
			fireGet(client, adserving.BuildClickURL(mc), traceparent)
		}
	}
	return impDelivered
}

// fireGet sends a tracker beacon GET with the same traceparent used for the
// auction (so the tracker span joins the auction trace) plus a browser-shaped
// User-Agent + Referer so the tracker's fraud check doesn't drop the headless
// client as a bot.
func fireGet(client *http.Client, url, traceparent string) bool {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("traceparent", traceparent)
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-simulator)")
	req.Header.Set("Referer", "https://simulator.dev/")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 300
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func printResults(sent, wins, errors int, elapsed time.Duration) {
	fmt.Println()
	fmt.Println("=== Simulation Results ===")
	fmt.Printf("  Duration:   %s\n", elapsed.Round(time.Second))
	fmt.Printf("  Requests:   %d\n", sent)
	fmt.Printf("  Wins:       %d (%.1f%%)\n", wins, pct(wins, sent))
	fmt.Printf("  No-fill:    %d\n", sent-wins-errors)
	if bl := atomic.LoadInt64(&beaconLost64); bl > 0 {
		fmt.Printf("  Beacon-lost: %d (wins with undelivered impression beacon — expect analytics = wins - this)\n", bl)
	}
	fmt.Printf("  Errors:     %d\n", errors)
	if elapsed.Seconds() > 0 {
		fmt.Printf("  Avg RPS:    %.1f\n", float64(sent)/elapsed.Seconds())
	}
	fmt.Println()
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d) * 100
}

func getFlag(name, defaultVal string) string {
	for i, arg := range os.Args {
		if arg == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return defaultVal
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Minute
	}
	return d
}

func parseInt(s string) int {
	n := 0
	fmt.Sscanf(s, "%d", &n)
	return n
}
