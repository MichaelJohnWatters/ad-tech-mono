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

	// Sort by price descending, with an explicit FAIR tie-break for equal bids.
	// Highest price wins (first-price). When two bids are exactly equal, we must
	// NOT let Go's unstable sort pick the winner — that arbitrarily hands one
	// bidder every tie. Instead rank equal bids by a uniform per-impression hash
	// of (impression, bidder), so equal bidders each win a fair, reproducible
	// share across impressions. Deterministic (no rand) — this runs in the hot
	// path and must stay fake-able; matches the pod/retail/batch convention of
	// SliceStable + an explicit tiebreak. Exact ties are rare with continuous
	// bids (flat CPM deals, floor-clamped bids), but when they happen this
	// removes the arbitrary-winner gap.
	sorted := make([]Bid, len(bids))
	copy(sorted, bids)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Price != sorted[j].Price {
			return sorted[i].Price > sorted[j].Price
		}
		return tieBreakKey(request.TraceID, sorted[i]) < tieBreakKey(request.TraceID, sorted[j])
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

// tieBreakKey is a uniform per-impression ordering key for a bid, used only to
// resolve EXACTLY-equal bids fairly. Because it mixes the impression's trace id
// with the bidder identity, equal bidders each win a uniform share as the trace
// id varies across impressions — fair rotation — while remaining fully
// deterministic for a given impression (reproducible, no rand).
func tieBreakKey(traceID string, b Bid) uint64 {
	// Inline FNV-1a over the strings — allocation-free (no hasher object, no
	// []byte conversion), so it stays cheap even if the sort comparator calls it
	// many times when lots of bids tie. Pure CPU, no I/O.
	const offset64 = uint64(14695981039346656037)
	const prime64 = uint64(1099511628211)
	h := offset64
	for i := 0; i < len(traceID); i++ {
		h = (h ^ uint64(traceID[i])) * prime64
	}
	h *= prime64 // field separator
	for i := 0; i < len(b.DSPID); i++ {
		h = (h ^ uint64(b.DSPID[i])) * prime64
	}
	h *= prime64
	for i := 0; i < len(b.CampaignID); i++ {
		h = (h ^ uint64(b.CampaignID[i])) * prime64
	}
	return h
}
