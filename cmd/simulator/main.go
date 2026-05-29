// cmd/simulator generates fake ad traffic for testing.
//
// Usage:
//
//	simulator run --profile steady --duration 5m
//	simulator run --profile trickle --requests 10
//	simulator single --geo UK --device mobile
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

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
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
  simulator single    Fire a single bid request
  simulator profiles  List available profiles
  simulator check     Check if services are reachable

Run flags:
  --profile <name>    Profile: trickle, steady, burst (default: trickle)
  --duration <dur>    Duration: 30s, 5m, 1h (default: 1m)
  --requests <n>      Stop after N requests (0 = use duration)
  --rps <n>           Override requests per second
  --exchange-url <u>  Exchange URL (default: http://localhost:8081)
  --tracker-url <u>   Tracker URL (default: http://localhost:8083)
  --geo <list>        Comma-separated geos (default: profile setting)
  --device <list>     Comma-separated devices (default: profile setting)

Single flags:
  --geo <geo>         Country code (default: GBR)
  --device <type>     Device type: mobile, desktop, tablet (default: mobile)
  --format <fmt>      Ad format: display, native (default: display)`)
}

type profile struct {
	Name           string
	RPS            int
	Geos           []string
	Devices        []string
	Formats        []string
	FloorPrice     float64
	ClickRate      float64
	ViewabilityPct int
}

var profiles = map[string]profile{
	"trickle": {
		Name: "trickle", RPS: 1,
		Geos: []string{"GBR"}, Devices: []string{"mobile"},
		Formats: []string{"display"}, FloorPrice: 0.50,
		ClickRate: 0.05, ViewabilityPct: 80,
	},
	"steady": {
		Name: "steady", RPS: 10,
		Geos: []string{"GBR", "USA", "DEU", "FRA", "JPN"},
		Devices: []string{"mobile", "desktop", "tablet"},
		Formats: []string{"display", "native"}, FloorPrice: 1.00,
		ClickRate: 0.02, ViewabilityPct: 70,
	},
	"burst": {
		Name: "burst", RPS: 100,
		Geos: []string{"GBR", "USA", "DEU", "FRA", "JPN", "AUS", "CAN", "BRA", "IND"},
		Devices: []string{"mobile", "desktop", "tablet", "ctv"},
		Formats: []string{"display", "native"}, FloorPrice: 0.50,
		ClickRate: 0.01, ViewabilityPct: 60,
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
	if geo := getFlag("--geo", ""); geo != "" {
		p.Geos = strings.Split(geo, ",")
	}
	if device := getFlag("--device", ""); device != "" {
		p.Devices = strings.Split(device, ",")
	}

	log.Info("simulation starting",
		"profile", p.Name,
		"rps", p.RPS,
		"duration", duration,
		"max_requests", maxRequests,
		"exchange", exchangeURL,
		"tracker", trackerURL,
	)

	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(time.Second / time.Duration(p.RPS))
	defer ticker.Stop()

	deadline := time.After(duration)
	sent := 0
	wins := 0
	errors := 0
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

			traceID := fmt.Sprintf("sim-%d-%d", time.Now().UnixMilli(), sent)
			bidReq := generateBidRequest(traceID, p)

			// Send auction request
			won, err := sendAuction(client, exchangeURL, bidReq)
			sent++
			if err != nil {
				errors++
				if sent <= 3 { // only log first few errors
					log.Error("auction failed", "error", err, "trace_id", traceID)
				}
				continue
			}

			if won {
				wins++
				// Fire impression pixel
				firePixel(client, trackerURL, traceID, "imp")

				// Simulate viewability (after delay in real life, instant here)
				if rand.Intn(100) < p.ViewabilityPct {
					fireViewability(client, trackerURL, traceID)
				}

				// Simulate click
				if rand.Float64() < p.ClickRate {
					fireClick(client, trackerURL, traceID)
				}
			}

			if sent%p.RPS == 0 { // log every second
				log.Info("progress",
					"sent", sent,
					"wins", wins,
					"errors", errors,
					"win_rate", fmt.Sprintf("%.1f%%", float64(wins)/float64(sent)*100),
					"elapsed", time.Since(start).Round(time.Second),
				)
			}
		}
	}
}

func runSingle() {
	geo := getFlag("--geo", "GBR")
	device := getFlag("--device", "mobile")
	exchangeURL := getFlag("--exchange-url", routes.DefaultExchangeURL)
	trackerURL := getFlag("--tracker-url", routes.DefaultTrackerURL)

	traceID := fmt.Sprintf("single-%d", time.Now().UnixMilli())
	bidReq := openrtb.BidRequest{
		ID:  traceID,
		Imp: []openrtb.Imp{{ID: "imp-1", Banner: &openrtb.Banner{W: 300, H: 250}, BidFloor: 1.0}},
		Site: &openrtb.Site{Domain: "test-publisher.com", Page: "https://test-publisher.com/test"},
		Device: &openrtb.Device{DeviceType: deviceTypeInt(device), Geo: &openrtb.Geo{Country: geo}},
		User: &openrtb.User{ID: "sim-user-001"},
		TMax: 100,
	}

	client := &http.Client{Timeout: 5 * time.Second}

	fmt.Printf("Trace ID: %s\n", traceID)
	fmt.Printf("Geo: %s | Device: %s | Floor: $1.00\n\n", geo, device)

	won, err := sendAuction(client, exchangeURL, bidReq)
	if err != nil {
		fmt.Printf("Auction: FAILED (%v)\n", err)
		return
	}

	if won {
		fmt.Println("Auction: WON")
		firePixel(client, trackerURL, traceID, "imp")
		fmt.Println("Impression: fired")
		fireViewability(client, trackerURL, traceID)
		fmt.Println("Viewability: fired")
	} else {
		fmt.Println("Auction: NO FILL")
	}

	fmt.Printf("\nDone. Trace ID: %s\n", traceID)
}

func listProfiles() {
	fmt.Println("Available profiles:")
	fmt.Println()
	for name, p := range profiles {
		fmt.Printf("  %-10s  %3d rps  geos: %-30s  devices: %s\n",
			name, p.RPS, strings.Join(p.Geos, ","), strings.Join(p.Devices, ","))
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
		fmt.Println("\nSome services are not reachable. Start them first:")
		fmt.Println("  go run ./cmd/dsp &")
		fmt.Println("  go run ./cmd/tracker &")
		fmt.Println("  go run ./cmd/exchange &")
		os.Exit(1)
	}
	fmt.Println("\nAll services reachable. Ready to simulate.")
}

func generateBidRequest(traceID string, p profile) openrtb.BidRequest {
	geo := p.Geos[rand.Intn(len(p.Geos))]
	device := p.Devices[rand.Intn(len(p.Devices))]
	userID := fmt.Sprintf("sim-user-%d", rand.Intn(1000))

	return openrtb.BidRequest{
		ID:  traceID,
		Imp: []openrtb.Imp{{ID: "imp-1", Banner: &openrtb.Banner{W: 300, H: 250}, BidFloor: p.FloorPrice}},
		Site: &openrtb.Site{
			Domain: "sim-publisher.com",
			Page:   fmt.Sprintf("https://sim-publisher.com/page/%d", rand.Intn(100)),
			Cat:    []string{"IAB17"},
		},
		Device: &openrtb.Device{
			DeviceType: deviceTypeInt(device),
			Geo:        &openrtb.Geo{Country: geo},
		},
		User: &openrtb.User{
			ID:  userID,
			Ext: &openrtb.UserExt{Segments: []string{"sim_segment"}},
		},
		TMax: 100,
	}
}

func sendAuction(client *http.Client, exchangeURL string, bidReq openrtb.BidRequest) (bool, error) {
	body, _ := json.Marshal(bidReq)
	resp, err := client.Post(exchangeURL+routes.OpenRTBAuction, constants.ContentTypeJSON, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var bidResp openrtb.BidResponse
	json.NewDecoder(resp.Body).Decode(&bidResp)
	return !bidResp.NoBid && len(bidResp.SeatBid) > 0, nil
}

func firePixel(client *http.Client, trackerURL, traceID, eventType string) {
	url := fmt.Sprintf("%s/v1/t/%s?tid=%s&cid=demo-campaign&pid=imp-1&sig=sim", trackerURL, eventType, traceID)
	client.Get(url)
}

func fireViewability(client *http.Client, trackerURL, traceID string) {
	dur := 1000 + rand.Intn(3000)
	pct := 50 + rand.Intn(50)
	url := fmt.Sprintf("%s/v1/t/view?tid=%s&dur=%d&pct=%d", trackerURL, traceID, dur, pct)
	client.Get(url)
}

func fireClick(client *http.Client, trackerURL, traceID string) {
	url := fmt.Sprintf("%s/v1/t/click?tid=%s&cid=demo-campaign&sig=sim&redir=https://example.com", trackerURL, traceID)
	client.Get(url)
}

func printResults(sent, wins, errors int, elapsed time.Duration) {
	winRate := 0.0
	if sent > 0 {
		winRate = float64(wins) / float64(sent) * 100
	}
	fmt.Println()
	fmt.Println("=== Simulation Results ===")
	fmt.Printf("  Duration:   %s\n", elapsed.Round(time.Second))
	fmt.Printf("  Requests:   %d\n", sent)
	fmt.Printf("  Wins:       %d (%.1f%%)\n", wins, winRate)
	fmt.Printf("  No-fill:    %d\n", sent-wins-errors)
	fmt.Printf("  Errors:     %d\n", errors)
	fmt.Printf("  Avg RPS:    %.1f\n", float64(sent)/elapsed.Seconds())
	fmt.Println()
}

func deviceTypeInt(s string) int {
	switch s {
	case "mobile":
		return 1
	case "desktop":
		return 2
	case "ctv":
		return 3
	case "tablet":
		return 5
	default:
		return 2
	}
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
