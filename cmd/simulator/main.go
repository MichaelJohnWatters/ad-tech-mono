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

Run flags:
  --profile <name>    Profile: trickle, steady, burst (default: trickle)
  --duration <dur>    Duration: 30s, 5m, 1h (default: 1m)
  --requests <n>      Stop after N requests (0 = use duration)
  --rps <n>           Override requests per second
  --exchange-url <u>  Exchange URL (default: http://localhost:8081)
  --tracker-url <u>   Tracker URL (default: http://localhost:8083)
  --persona <name>    Force a single persona (see 'simulator personas')
  --channel <ch>      Force a channel: display, video, audio, native
  --geo <geo>         Override geo on every request (ISO alpha-3)
  --device <type>     Override device: mobile, desktop, tablet, ctv

Single flags:
  --persona <name>    Persona (default: us-personalised-mobile)
  --channel <ch>      Channel: display, video, audio, native (default: display)
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
	// regimes, and display/native/video.
	"steady": {
		Name: "steady", RPS: 10,
		Personas:   request.Personas, // full registry, weighted
		Channels:   []channelWeight{{request.Display, 60}, {request.Native, 25}, {request.Video, 15}},
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
			bidReq := request.Build(request.Input{
				TraceID: traceID, Channel: channel, Persona: persona,
				Placement: simPlacement(p.FloorPrice), Rand: rng,
			})

			won, err := sendAuction(client, exchangeURL, bidReq, traceparent)
			sent++
			if err != nil {
				errors++
				if sent <= 3 {
					log.Error("auction failed", "error", err, "trace_id", traceID)
				}
				continue
			}
			if won {
				wins++
				fireEvents(client, trackerURL, traceID, traceparent, channel, p, rng)
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

	persona, ok := request.PersonaByName(personaName)
	if !ok {
		fmt.Printf("Unknown persona: %s\n", personaName)
		listPersonas()
		os.Exit(1)
	}
	applyOverrides(&persona, getFlag("--geo", ""), getFlag("--device", ""))

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	traceID, traceparent := tracing.NewClientTraceparent()
	bidReq := request.Build(request.Input{
		TraceID: traceID, Channel: channel, Persona: persona,
		Placement: simPlacement(1.0), Rand: rng,
	})

	client := &http.Client{Timeout: 5 * time.Second}

	fmt.Printf("Trace ID: %s\n", traceID)
	fmt.Printf("Persona:  %s (%s, %s, id=%s, consent=%s)\n",
		persona.Name, persona.Geo, persona.Device, persona.Identity, persona.Regime)
	fmt.Printf("Channel:  %s\n\n", channel)

	won, err := sendAuction(client, exchangeURL, bidReq, traceparent)
	if err != nil {
		fmt.Printf("Auction: FAILED (%v)\n", err)
		return
	}
	if won {
		fmt.Println("Auction: WON")
		fireEvents(client, trackerURL, traceID, traceparent, channel, profiles["trickle"], rng)
		fmt.Println("Events:  fired")
	} else {
		fmt.Println("Auction: NO FILL")
	}
	fmt.Printf("\nDone. Trace ID: %s\n", traceID)
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

func sendAuction(client *http.Client, exchangeURL string, bidReq openrtb.BidRequest, traceparent string) (bool, error) {
	body, _ := json.Marshal(bidReq)
	req, err := http.NewRequest("POST", exchangeURL+routes.OpenRTBAuction, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", constants.ContentTypeJSON)
	req.Header.Set("traceparent", traceparent)
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var bidResp openrtb.BidResponse
	json.NewDecoder(resp.Body).Decode(&bidResp)
	return !bidResp.NoBid && len(bidResp.SeatBid) > 0, nil
}

// fireEvents fires the post-win beacon sequence appropriate to the channel:
// an impression for every format, then video/audio quartile events for
// instream, and a stochastic viewability + click for display/native.
func fireEvents(client *http.Client, trackerURL, traceID, traceparent string, ch request.Channel, p profile, rng *rand.Rand) {
	firePixel(client, trackerURL, traceID, traceparent, "imp")

	switch ch {
	case request.Video:
		for _, ev := range []string{"start", "firstQuartile", "midpoint", "thirdQuartile", "complete"} {
			fireMediaEvent(client, trackerURL, traceID, traceparent, "video", ev)
		}
		if rng.Float64() < p.ClickRate {
			fireClick(client, trackerURL, traceID, traceparent)
		}
	case request.Audio:
		for _, ev := range []string{"start", "firstQuartile", "midpoint", "thirdQuartile", "complete"} {
			fireMediaEvent(client, trackerURL, traceID, traceparent, "audio", ev)
		}
	default: // display, native
		if rng.Intn(100) < p.ViewPct {
			fireViewability(client, trackerURL, traceID, traceparent)
		}
		if rng.Float64() < p.ClickRate {
			fireClick(client, trackerURL, traceID, traceparent)
		}
	}
}

// fireGet sends a tracker pixel GET with the same traceparent used for the
// auction (so the tracker span joins the auction trace) plus a browser-shaped
// User-Agent + Referer so the tracker's fraud check doesn't drop it as a bot.
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

func firePixel(client *http.Client, trackerURL, traceID, traceparent, eventType string) {
	url := fmt.Sprintf("%s/v1/t/%s?tid=%s&cid=demo-campaign&pid=imp-1&sig=sim", trackerURL, eventType, traceID)
	fireGet(client, url, traceparent)
}

func fireMediaEvent(client *http.Client, trackerURL, traceID, traceparent, kind, event string) {
	url := fmt.Sprintf("%s/v1/t/%s?tid=%s&event=%s&sig=sim", trackerURL, kind, traceID, event)
	fireGet(client, url, traceparent)
}

func fireViewability(client *http.Client, trackerURL, traceID, traceparent string) {
	dur := 1000 + rand.Intn(3000)
	pct := 50 + rand.Intn(50)
	url := fmt.Sprintf("%s/v1/t/view?tid=%s&dur=%d&pct=%d", trackerURL, traceID, dur, pct)
	fireGet(client, url, traceparent)
}

func fireClick(client *http.Client, trackerURL, traceID, traceparent string) {
	url := fmt.Sprintf("%s/v1/t/click?tid=%s&cid=demo-campaign&sig=sim&redir=https://example.com", trackerURL, traceID)
	fireGet(client, url, traceparent)
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
