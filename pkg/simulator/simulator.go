// Package simulator provides a programmable ad traffic simulator
// as a Go library. Used by the CLI tool and available as an HTTP API.
//
// Usage:
//
//	sim := simulator.New(simulator.Config{ExchangeURL: "http://localhost:8081"})
//	result := sim.RunSingle(ctx, simulator.Request{Geo: "GBR", Device: "mobile"})
//	results := sim.RunProfile(ctx, "steady", 5*time.Minute)
package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// Config holds simulator configuration.
type Config struct {
	ExchangeURL string
	TrackerURL  string
	AdServerURL string
	Timeout     time.Duration
}

// DefaultConfig returns local dev defaults.
func DefaultConfig() Config {
	return Config{
		ExchangeURL: "http://localhost:8081",
		TrackerURL:  "http://localhost:8083",
		AdServerURL: "http://localhost:8085",
		Timeout:     5 * time.Second,
	}
}

// Request defines a single simulated ad request.
type Request struct {
	Geo       string
	Device    string // mobile, desktop, tablet
	Domain    string
	BidFloor  float64
	Width     int
	Height    int
	UserID    string
}

// Result holds the outcome of a simulated request.
type Result struct {
	TraceID      string
	HasWinner    bool
	WinnerDSP    string
	WinPrice     float64
	CampaignID   string
	CreativeID   string
	AuctionMs    int64
	PixelFired   bool
	Error        string
}

// ProfileResult holds aggregate results from a simulation profile run.
type ProfileResult struct {
	Profile     string
	Duration    time.Duration
	TotalSent   int64
	TotalWins   int64
	TotalNoBid  int64
	TotalErrors int64
	AvgLatency  time.Duration
	MaxLatency  time.Duration
}

// Simulator runs ad traffic simulations.
type Simulator struct {
	config Config
	client *http.Client
}

// New creates a simulator.
func New(cfg Config) *Simulator {
	return &Simulator{
		config: cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}
}

// RunSingle fires a single auction request.
func (s *Simulator) RunSingle(ctx context.Context, req Request) Result {
	// W3C-formatted trace ID + matching traceparent so the exchange's
	// HTTPMiddleware adopts this exact trace ID (rather than generating a
	// fresh one). The same ID ends up in Jaeger, Loki, NATS events, and the
	// analytics store — one ID to paste into Grafana.
	traceID, traceparent := tracing.NewClientTraceparent()
	if req.Width == 0 {
		req.Width = 300
	}
	if req.Height == 0 {
		req.Height = 250
	}
	if req.BidFloor == 0 {
		req.BidFloor = 0.50
	}
	if req.Domain == "" {
		req.Domain = "sim-publisher.com"
	}

	bidReq := map[string]interface{}{
		"id": traceID,
		"imp": []map[string]interface{}{{
			"id":       "imp-1",
			"banner":   map[string]int{"w": req.Width, "h": req.Height},
			"bidfloor": req.BidFloor,
		}},
		"site": map[string]string{"domain": req.Domain},
		"device": map[string]interface{}{
			"devicetype": deviceTypeInt(req.Device),
			"geo":        map[string]string{"country": req.Geo},
		},
		"tmax": 100,
	}

	body, _ := json.Marshal(bidReq)
	start := time.Now()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", s.config.ExchangeURL+"/v1/openrtb/auction", bytes.NewReader(body))
	if err != nil {
		return Result{TraceID: traceID, Error: err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("traceparent", traceparent)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return Result{TraceID: traceID, Error: err.Error(), AuctionMs: time.Since(start).Milliseconds()}
	}
	defer resp.Body.Close()

	var bidResp struct {
		SeatBid []struct {
			Seat string `json:"seat"`
			Bid  []struct {
				Price float64 `json:"price"`
				CID   string  `json:"cid"`
				CrID  string  `json:"crid"`
			} `json:"bid"`
		} `json:"seatbid"`
		NoBid bool `json:"nobid"`
	}
	json.NewDecoder(resp.Body).Decode(&bidResp)

	result := Result{
		TraceID:   traceID,
		AuctionMs: time.Since(start).Milliseconds(),
	}

	if bidResp.NoBid || len(bidResp.SeatBid) == 0 || len(bidResp.SeatBid[0].Bid) == 0 {
		return result
	}

	result.HasWinner = true
	result.WinnerDSP = bidResp.SeatBid[0].Seat
	result.WinPrice = bidResp.SeatBid[0].Bid[0].Price
	result.CampaignID = bidResp.SeatBid[0].Bid[0].CID
	result.CreativeID = bidResp.SeatBid[0].Bid[0].CrID

	// Fire impression pixel with the same traceparent so the tracker's
	// span joins the auction trace in Jaeger.
	pixelURL := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=imp-1&pubid=sim-pub&price=%.4f&cur=USD",
		s.config.TrackerURL, traceID, result.CampaignID, result.CreativeID, result.WinPrice)
	if pixReq, err := http.NewRequestWithContext(ctx, "GET", pixelURL, nil); err == nil {
		pixReq.Header.Set("traceparent", traceparent)
		// Browser-shaped UA + Referer so the tracker's fraud check accepts
		// the request (default Go UA = bot, drops the impression). Mirrors
		// cmd/simulator/main.go:fireGet and tests/e2e/harness/tracker.go.
		pixReq.Header.Set("User-Agent", "Mozilla/5.0 (adtech-simulator)")
		pixReq.Header.Set("Referer", "https://simulator.dev/")
		if pixResp, err := s.client.Do(pixReq); err == nil {
			pixResp.Body.Close()
			result.PixelFired = true
		}
	}

	return result
}

// RunProfile runs a named simulation profile for the given duration.
func (s *Simulator) RunProfile(ctx context.Context, profile string, duration time.Duration) ProfileResult {
	rps := profileRPS(profile)
	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()

	deadline := time.After(duration)
	pr := ProfileResult{Profile: profile}

	var totalLatency int64
	var maxLatency int64
	var mu sync.Mutex

	for {
		select {
		case <-ctx.Done():
			goto done
		case <-deadline:
			goto done
		case <-ticker.C:
			go func() {
				req := randomRequest()
				result := s.RunSingle(ctx, req)

				mu.Lock()
				atomic.AddInt64(&pr.TotalSent, 1)
				if result.HasWinner {
					atomic.AddInt64(&pr.TotalWins, 1)
				} else if result.Error != "" {
					atomic.AddInt64(&pr.TotalErrors, 1)
				} else {
					atomic.AddInt64(&pr.TotalNoBid, 1)
				}
				totalLatency += result.AuctionMs
				if result.AuctionMs > maxLatency {
					maxLatency = result.AuctionMs
				}
				mu.Unlock()
			}()
		}
	}
done:
	pr.Duration = duration
	if pr.TotalSent > 0 {
		pr.AvgLatency = time.Duration(totalLatency/pr.TotalSent) * time.Millisecond
	}
	pr.MaxLatency = time.Duration(maxLatency) * time.Millisecond
	return pr
}

func profileRPS(profile string) int {
	switch profile {
	case "trickle":
		return 1
	case "steady":
		return 10
	case "burst":
		return 100
	case "stress":
		return 500
	default:
		return 5
	}
}

func randomRequest() Request {
	geos := []string{"GBR", "USA", "DEU", "FRA", "JPN"}
	devices := []string{"mobile", "desktop", "tablet"}
	return Request{
		Geo:      geos[rand.Intn(len(geos))],
		Device:   devices[rand.Intn(len(devices))],
		BidFloor: 0.50 + rand.Float64()*2.0,
	}
}

func deviceTypeInt(device string) int {
	switch device {
	case "mobile":
		return 1
	case "desktop":
		return 2
	case "tablet":
		return 5
	default:
		return 2
	}
}
