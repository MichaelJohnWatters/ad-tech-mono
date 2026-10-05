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
