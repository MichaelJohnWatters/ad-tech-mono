package main

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// sampleTrace deterministically decides whether a trace is in the emit sample
// for a given ratio (0..1). Same trace_id → same decision, so all of an
// auction's per-DSP events are emitted together or not at all. No RNG, so it's
// reproducible and lock-free.
func sampleTrace(traceID string, ratio float64) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(traceID))
	return float64(h.Sum32()%10000)/10000.0 < ratio
}

// dspCallStat mirrors analytics.DSPCallStat's JSON — a local copy so the
// exchange doesn't import the analytics package (and its ClickHouse driver)
// just to decode the warm-start response.
type dspCallStat struct {
	Channel       string  `json:"channel"`
	DSPEndpoint   string  `json:"dsp_endpoint"`
	TotalCalls    int64   `json:"total_calls"`
	TotalBids     int64   `json:"total_bids"`
	TotalTimeouts int64   `json:"total_timeouts"`
	AvgBidUSD     float64 `json:"avg_bid_usd"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
}

// warmStartRouter seeds the SmartRouter from reporting's dsp_calls aggregate so
// routing survives a restart with real history instead of re-learning from cold
// (ADR 0003). Fail-open: any error logs and leaves the router cold. Run in a
// goroutine at boot — Seed is mutex-guarded, so it's safe to land after the
// handler starts serving; a cold router just isn't worse than today.
func warmStartRouter(cfg *config.Config, router *optimise.SmartRouter, log *slog.Logger) {
	if !cfg.GetBool("exchange.routing_warmstart", true) {
		return
	}
	base := cfg.Get("exchange.reporting_url", "http://localhost:"+routes.PortReporting)
	url := base + "/debug/routing/stats?since_hours=6"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Warn("routing warm-start skipped: bad request", "error", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn("routing warm-start skipped: reporting unreachable", "url", url, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn("routing warm-start skipped: reporting non-200", "status", resp.StatusCode)
		return
	}
	var stats []dspCallStat
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		log.Warn("routing warm-start skipped: decode failed", "error", err)
		return
	}
	seed := make([]optimise.DSPStats, 0, len(stats))
	for _, s := range stats {
		ds := optimise.DSPStats{
			Channel: s.Channel, DSPID: s.DSPEndpoint,
			TotalCalls: s.TotalCalls, TotalBids: s.TotalBids, TotalTimeouts: s.TotalTimeouts,
			AvgBid: s.AvgBidUSD, AvgLatency: time.Duration(s.AvgLatencyMs * float64(time.Millisecond)),
		}
		if s.TotalCalls > 0 {
			ds.BidRate = float64(s.TotalBids) / float64(s.TotalCalls)
			ds.TimeoutRate = float64(s.TotalTimeouts) / float64(s.TotalCalls)
		}
		// WinRate not derivable from dsp_calls alone — re-learned live.
		seed = append(seed, ds)
	}
	router.Seed(seed)
	log.Info("routing warm-started from reporting dsp_calls", "endpoints", len(seed))
}
