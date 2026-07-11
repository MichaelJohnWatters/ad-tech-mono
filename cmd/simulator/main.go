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
	"strings"
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
  --rps <n>           Override requests per second
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
		FloorPrice: 0.50, ClickRate: 0.05, ViewPct: 80,
	},
	// steady: a broad, realistic open-exchange blend across regions, consent
	// regimes, and every serving format (display/native/video/audio).
	"steady": {
		Name: "steady", RPS: 10,
		Personas:   request.Personas, // full registry, weighted
		Channels:   []channelWeight{{request.Display, 55}, {request.Native, 20}, {request.Video, 15}, {request.Audio, 10}},
		FloorPrice: 1.00, ClickRate: 0.02, ViewPct: 70,
	},
	// burst: high volume across every channel including audio + CTV personas.
	"burst": {
		Name: "burst", RPS: 100,
		Personas:   request.Personas,
		Channels:   []channelWeight{{request.Display, 45}, {request.Video, 25}, {request.Audio, 15}, {request.Native, 15}},
		FloorPrice: 0.50, ClickRate: 0.01, ViewPct: 60,
	},
}

func runSimulation() {
	profileName := getFlag("--profile", "trickle")
	duration := parseDuration(getFlag("--duration", "1m"))
	maxRequests := parseInt(getFlag("--requests", "0"))
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
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(time.Second / time.Duration(p.RPS))
	defer ticker.Stop()

	deadline := time.After(duration)
	sent, wins, errors := 0, 0, 0
	start := time.Now()

	for {
		select {
		case <-deadline:
			printResults(sent, wins, errors, time.Since(start))
			return
		case <-ticker.C:
			if maxRequests > 0 && sent >= maxRequests {
				printResults(sent, wins, errors, time.Since(start))
				return
			}

			traceID, traceparent := tracing.NewClientTraceparent()
			persona := request.Pick(rng, p.Personas)
			applyOverrides(&persona, geoOverride, deviceOverride)
			channel := pickChannel(rng, p.Channels)

			served, err := runOne(client, eps, exchangeURL, trackerURL, persona, channel, podSize, p, rng, traceID, traceparent, directMode)
			sent++
			if err != nil {
				errors++
				if sent <= 3 {
					log.Error("request failed", "error", err, "trace_id", traceID)
				}
				continue
			}
			if served {
				wins++
			}

			if sent%p.RPS == 0 {
				log.Info("progress",
					"sent", sent, "wins", wins, "errors", errors,
					"win_rate", fmt.Sprintf("%.1f%%", pct(wins, sent)),
					"elapsed", time.Since(start).Round(time.Second),
				)
			}
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
	fireEvents(client, trackerURL, traceID, traceparent, ch, winner, placement, p, rng)
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
func fireEvents(client *http.Client, trackerURL, traceID, traceparent string, ch request.Channel, winner *openrtb.BidObj, pl request.Placement, p profile, rng *rand.Rand) {
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

	fireGet(client, adserving.BuildImpressionURL(mc), traceparent)

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
}

// fireGet sends a tracker beacon GET with the same traceparent used for the
// auction (so the tracker span joins the auction trace) plus a browser-shaped
// User-Agent + Referer so the tracker's fraud check doesn't drop the headless
// client as a bot.
func fireGet(client *http.Client, url, traceparent string) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	req.Header.Set("traceparent", traceparent)
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-simulator)")
	req.Header.Set("Referer", "https://simulator.dev/")
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
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
