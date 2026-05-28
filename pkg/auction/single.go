package auction

import (
	"context"
	"sort"
)

// SingleWinnerStrategy selects the highest bidder. Used for display, native,
// single video ads, rewarded, and interstitial.
//
// First-price: winner pays their bid.
// Second-price: winner pays second-highest bid + $0.01 (or floor if only one bid).
type SingleWinnerStrategy struct{}

func (s *SingleWinnerStrategy) Type() string { return "single_winner" }

func (s *SingleWinnerStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	if len(bids) == 0 {
		return Result{}, ErrNoBids
	}

	// Sort by price descending
	sorted := make([]Bid, len(bids))
	copy(sorted, bids)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Price > sorted[j].Price
	})

	winner := sorted[0]

	// Calculate clearing price
	var clearingPrice float64
	switch request.PriceMode {
	case "second_price":
		if len(sorted) > 1 {
			clearingPrice = sorted[1].Price + 0.01
		} else {
			clearingPrice = request.FloorPrice
		}
		// Clearing price can't exceed the winning bid
		if clearingPrice > winner.Price {
			clearingPrice = winner.Price
		}
	default: // first_price (default)
		clearingPrice = winner.Price
	}

	// Build result
	result := Result{
		Winners: []Winner{
			{
				Bid:           winner,
				ClearingPrice: clearingPrice,
				Position:      1,
				TraceID:       request.TraceID,
			},
		},
		IsMultiWinner: false,
	}

	// All other bids are losers
	for i := 1; i < len(sorted); i++ {
		result.LossBids = append(result.LossBids, LossBid{
			Bid:        sorted[i],
			Reason:     LossOutbid,
			ReasonText: "outbid by higher bidder",
		})
	}

	return result, nil
}
