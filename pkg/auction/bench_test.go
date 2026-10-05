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
