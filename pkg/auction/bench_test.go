package auction_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// BenchmarkExchangeAuction is the exchange's compute core: winner selection
// over the fan-out bid set (pkg/auction.Engine.RunAuction — the same call the
// exchange handler makes once per auction, minus the DSP network fan-out that
// produced the bids). Pure + allocation-visible (run with -benchmem), so a
// regression here attributes to a commit at PR time rather than surfacing as
// an unexplained "fanout p95 up" in a stack run.
//
// Swept over bid-set sizes because selection cost scales with the number of
// competing bids, which grows with the DSP roster + multi-seat responses.
func BenchmarkExchangeAuction(b *testing.B) {
	for _, n := range []int{2, 5, 25, 100} {
		b.Run(fmt.Sprintf("bids=%d", n), func(b *testing.B) {
			engine := auction.NewEngine(clock.Real{})
			bids := make([]auction.Bid, n)
			for i := range bids {
				bids[i] = auction.Bid{
					DSPID:        fmt.Sprintf("dsp_%d", i),
					CampaignID:   fmt.Sprintf("camp_%d", i),
					AdvertiserID: fmt.Sprintf("adv_%d", i%7), // some shared advertisers → separation work
					Price:        1.0 + float64(i%50)/10.0,
				}
			}
			req := auction.AuctionRequest{
				Channel:    "display",
				PriceMode:  "first_price",
				FloorPrice: 1.00,
				TraceID:    "bench-trace",
			}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := engine.RunAuction(ctx, bids, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkExchangeAuctionThroughput reports the COMPUTE-CEILING throughput of
// winner selection: RunAuction driven across all GOMAXPROCS cores with zero
// I/O, reported as auctions/sec. This answers "how fast could the auction MATH
// go" — crank it with `-cpu 1,2,4,8 -benchtime=10s` to watch it scale with
// cores on a generous box, no cluster needed.
//
// CRITICAL: this is NOT platform throughput. The live system is I/O-bound (DSP
// fan-out over the wire, Redis, NATS, JSON, GC under concurrency) and tops out
// far lower — see docs/perf/runs.jsonl / `make perfbench` for the real number.
// This ceiling just proves the auction compute has headroom to spare and will
// never be the bottleneck; use perfbench for capacity planning.
func BenchmarkExchangeAuctionThroughput(b *testing.B) {
	engine := auction.NewEngine(clock.Real{})
	bids := make([]auction.Bid, 25)
	for i := range bids {
		bids[i] = auction.Bid{
			DSPID: fmt.Sprintf("dsp_%d", i), CampaignID: fmt.Sprintf("camp_%d", i),
			AdvertiserID: fmt.Sprintf("adv_%d", i%7), Price: 1.0 + float64(i%50)/10.0,
		}
	}
	req := auction.AuctionRequest{Channel: "display", PriceMode: "first_price", FloorPrice: 1.00, TraceID: "bench-trace"}
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := engine.RunAuction(ctx, bids, req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "auctions/sec")
}

// BenchmarkExchangeAuctionProfiles benches DISTINCT auction shapes — each
// exercises a different strategy + code path, not just a bigger input:
//   - display-open:  single-winner first-price (the common display auction)
//   - retail-grid:   relevance-weighted MULTI-winner sponsored-product grid
//     (ranks Relevance×Price, fills SlotCount positions)
//
// Add a row here to profile another ad type (video pod, DOOH time-slot, …) —
// the engine picks the strategy from the request, so a profile is just the
// right AuctionRequest + bid shape. See the skill for the full menu.
func BenchmarkExchangeAuctionProfiles(b *testing.B) {
	engine := auction.NewEngine(clock.Real{})
	ctx := context.Background()

	profiles := []struct {
		name string
		bids []auction.Bid
		req  auction.AuctionRequest
	}{
		{
			name: "display-open",
			bids: mkBids(25, false),
			req:  auction.AuctionRequest{Channel: "display", PriceMode: "first_price", FloorPrice: 1.00, TraceID: "p"},
		},
		{
			name: "retail-grid",
			bids: mkBids(25, true), // carry Category + Relevance
			req: auction.AuctionRequest{
				Channel: "retail", SlotCount: 5, RetailCategories: []string{"IAB18", "IAB18-5"},
				FloorPrice: 0.50, TraceID: "p",
			},
		},
	}
	for _, p := range profiles {
		b.Run(p.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := engine.RunAuction(ctx, p.bids, p.req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// mkBids builds n synthetic bids; retail=true adds Category + Relevance so the
// relevance-weighted strategy has real ranking work.
func mkBids(n int, retail bool) []auction.Bid {
	cats := []string{"IAB18-5", "IAB18", "IAB18-1", "IAB22"}
	bids := make([]auction.Bid, n)
	for i := range bids {
		bids[i] = auction.Bid{
			DSPID: fmt.Sprintf("dsp_%d", i), CampaignID: fmt.Sprintf("camp_%d", i),
			AdvertiserID: fmt.Sprintf("adv_%d", i%7), Price: 1.0 + float64(i%50)/10.0,
		}
		if retail {
			bids[i].Category = cats[i%len(cats)]
			bids[i].Relevance = 0.2 + float64(i%8)/10.0
		}
	}
	return bids
}

// BenchmarkExchangeAuctionSecondPrice covers the second-price clearing path
// (a different code branch: it must find the runner-up, not just the max).
func BenchmarkExchangeAuctionSecondPrice(b *testing.B) {
	engine := auction.NewEngine(clock.Real{})
	bids := make([]auction.Bid, 25)
	for i := range bids {
		bids[i] = auction.Bid{
			DSPID:        fmt.Sprintf("dsp_%d", i),
			CampaignID:   fmt.Sprintf("camp_%d", i),
			AdvertiserID: fmt.Sprintf("adv_%d", i%7),
			Price:        1.0 + float64(i%50)/10.0,
		}
	}
	req := auction.AuctionRequest{Channel: "display", PriceMode: "second_price", FloorPrice: 1.00, TraceID: "bench-trace"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := engine.RunAuction(ctx, bids, req); err != nil {
			b.Fatal(err)
		}
	}
}
