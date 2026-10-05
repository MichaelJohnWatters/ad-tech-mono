package main

import (
	"fmt"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
)

// BenchmarkDSPBidPath is the DSP's compute core: the per-campaign bid-decision
// loop the hot-path iron rule keeps I/O-free (campaigns + budgets live in the
// warm in-process cache, refreshed in the background — the bid loop reads
// copies). This benches the real work that loop does for every campaign on
// every request — size-aware creative selection (selectCreativeForSize) and
// targeting evaluation (pkg/targeting.Evaluate) — over a warm set of N
// campaigns against one bid request.
//
// It deliberately excludes the Redis/Postgres the handler does OUTSIDE the
// loop (balance gate, segment lookup) — those are network and belong to the
// stack-level harness. Swept over catalog size because this is O(campaigns):
// catalog growth IS the load (per the perf-loadtest skill), so a per-campaign
// regression multiplies by the whole book. Run with -benchmem — allocations
// in this loop are the usual culprit.
func BenchmarkDSPBidPath(b *testing.B) {
	for _, n := range []int{10, 50, 200} {
		b.Run(fmt.Sprintf("campaigns=%d", n), func(b *testing.B) {
			campaigns := makeBenchCampaigns(n)
			req := targeting.Request{
				Geo: "USA", Device: "mobile", OS: "iOS",
				Segments: []string{"sports_fans", "auto_intenders"},
				Domain:   "demo-news.example", Channel: "display",
				Categories: []string{"IAB1", "IAB17"},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				eligible := 0
				for j := range campaigns {
					c := &campaigns[j]
					// Mirror the loop's cheap gates in order: creative size
					// match, then targeting. A non-match short-circuits, same
					// as the handler.
					if selectCreativeForSize(c, 300, 250) == "" {
						continue
					}
					if !targeting.Evaluate(c.Targeting, req).Matched {
						continue
					}
					eligible++
				}
				_ = eligible
			}
		})
	}
}

// BenchmarkDSPBidPathThroughput reports the COMPUTE-CEILING throughput of the
// per-request bid decision (the whole warm-cache campaign loop) across all
// GOMAXPROCS cores, as bids-evaluated/sec — "how fast could the bid MATH go"
// with zero I/O. Crank with `-cpu 1,2,4,8 -benchtime=10s`.
//
// CRITICAL: NOT platform throughput — the live DSP is gated by the network
// (exchange fan-out), Redis/Postgres warm-cache refresh, and GC under load,
// not this loop. Real rps lives in `make perfbench`. This ceiling just shows
// the bid loop has headroom (and, at 0 allocs/op, won't pressure GC).
func BenchmarkDSPBidPathThroughput(b *testing.B) {
	campaigns := makeBenchCampaigns(200)
	req := targeting.Request{
		Geo: "USA", Device: "mobile", OS: "iOS",
		Segments: []string{"sports_fans", "auto_intenders"},
		Domain:   "demo-news.example", Channel: "display",
		Categories: []string{"IAB1", "IAB17"},
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			eligible := 0
			for j := range campaigns {
				c := &campaigns[j]
				if selectCreativeForSize(c, 300, 250) == "" {
					continue
				}
				if !targeting.Evaluate(c.Targeting, req).Matched {
					continue
				}
				eligible++
			}
			_ = eligible
		}
	})
	// Each op is one full request evaluated against the whole book.
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "bidreqs/sec")
}

// BenchmarkDSPBidPathProfiles benches the bid loop against DISTINCT request
// styles — the cost of targeting.Evaluate depends on how much targeting a
// request/book actually carries, not just the campaign count:
//   - broad:  geo/device only (most campaigns match cheaply)
//   - dense:  geo+device+segments+categories+keywords all evaluated
//   - video:  non-display format path (creative falls back to primary, video channel)
//
// All run against the same 200-campaign warm set. Add a row to profile another
// channel/targeting style (native, retail relevance, DOOH) — see the skill.
func BenchmarkDSPBidPathProfiles(b *testing.B) {
	campaigns := makeBenchCampaigns(200)
	profiles := map[string]targeting.Request{
		"broad": {Geo: "USA", Device: "mobile", Channel: "display"},
		"dense": {
			Geo: "USA", Device: "mobile", OS: "iOS",
			Segments:   []string{"sports_fans", "auto_intenders", "in_market_auto"},
			Categories: []string{"IAB1", "IAB17", "IAB3"},
			Keywords:   []string{"ev", "sedan", "lease"},
			Domain:     "demo-news.example", Channel: "display",
		},
		"video": {Geo: "USA", Device: "ctv", Channel: "video"},
	}
	for name, req := range profiles {
		b.Run(name, func(b *testing.B) {
			reqW, reqH := 300, 250
			if req.Channel != "display" {
				reqW, reqH = 0, 0 // non-display: creative falls back to primary
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				eligible := 0
				for j := range campaigns {
					c := &campaigns[j]
					if selectCreativeForSize(c, reqW, reqH) == "" {
						continue
					}
					if !targeting.Evaluate(c.Targeting, req).Matched {
						continue
					}
					eligible++
				}
				_ = eligible
			}
		})
	}
}

// makeBenchCampaigns builds a realistic warm-cache campaign set: a mix of
// matching and non-matching targeting + a 300x250 creative variant, so both
// the short-circuit (no creative / excluded) and the full-match paths are
// exercised the way a real book does.
func makeBenchCampaigns(n int) []models.Campaign {
	out := make([]models.Campaign, n)
	geos := []string{"USA", "GBR", "CAN", "AUS"}
	for i := 0; i < n; i++ {
		incGeo := geos[i%len(geos)]
		out[i] = models.Campaign{
			ID:         fmt.Sprintf("li-%d", i),
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
					Geo:    []string{incGeo},
					Device: []string{"mobile", "desktop"},
					// Every 4th campaign gates on a segment the request has;
					// the rest match on geo/device alone — a realistic spread
					// of broad vs. audience-targeted line items.
					Segments: segmentsFor(i),
				},
				Exclude: targeting.TargetingSet{
					Categories: []string{"IAB7"}, // health exclusion — common
				},
			},
		}
	}
	return out
}

func segmentsFor(i int) []string {
	switch i % 4 {
	case 0:
		return []string{"auto_intenders"}
	case 1:
		return []string{"luxury_watch_intent"} // request lacks this → no-match path
	default:
		return nil
	}
}
