package auction

import (
	"context"
	"sort"
	"strings"
)

// RelevanceWeightedStrategy ranks sponsored products for a retail-media results
// or browse page. Unlike a display auction (pure highest bid), retail ranks by
// relevance × bid — so a highly relevant product with a lower bid can outrank an
// irrelevant product bidding more. This is the sponsored-products model retailers
// run: the shopper still sees products that match what they're looking at, and the
// retailer maximises long-run yield rather than a single high-but-irrelevant bid.
//
// It's a multi-winner auction: a results page has SlotCount sponsored slots, so
// the top-SlotCount ranked bids each win a slot (Position 1..N, position 1 =
// highest score). Each winner clears first-price (pays its own bid) — generalized
// second-price (each pays the minimum to hold its rank, as in search ads) is a
// future refinement noted in docs/PLAN.md.
//
// Relevance per bid:
//   - Bid.Relevance (0..1) when the caller supplies an explicit score (e.g. an
//     ML relevance model), else
//   - derived from Bid.Category vs the request's RetailCategories (the shopper's
//     browsed/searched categories): a category match is fully relevant, a miss is
//     heavily discounted but non-zero (an unmatched product can still fill an
//     otherwise-empty slot). When the request carries no category signal, every
//     bid is relevance-neutral and ranking degrades to pure price.
//
// See docs/PLAN.md -> "Retail Media" and "Unified Auction Engine".
type RelevanceWeightedStrategy struct{}

func (s *RelevanceWeightedStrategy) Type() string { return "relevance_weighted" }

// relevanceMiss is the score multiplier for a sponsored product whose category
// doesn't match what the shopper is looking at. Low enough that a matched product
// beats it at a much lower bid, non-zero so it can still fill an empty slot.
const relevanceMiss = 0.1

func (s *RelevanceWeightedStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	if len(bids) == 0 {
		return Result{}, ErrNoBids
	}

	slots := request.SlotCount
	if slots < 1 {
		slots = 1
	}

	type scored struct {
		bid   Bid
		rel   float64
		score float64
	}
	result := Result{IsMultiWinner: true}
	var ranked []scored
	for _, b := range bids {
		rel := retailRelevance(request, b)
		// Eligibility floor: too irrelevant to show, even at a high bid.
		if request.MinRelevance > 0 && rel < request.MinRelevance {
			result.LossBids = append(result.LossBids, LossBid{
				Bid: b, Reason: LossBlocked, ReasonText: "below minimum relevance",
			})
			continue
		}
		ranked = append(ranked, scored{bid: b, rel: rel, score: rel * b.Price})
	}
	if len(ranked) == 0 {
		return result, ErrNoBids
	}
	// Highest relevance-weighted score wins; ties break by raw bid (the retailer
	// prefers the higher-paying of two equally-ranked products).
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].bid.Price > ranked[j].bid.Price
	})

	n := slots
	if n > len(ranked) {
		n = len(ranked)
	}

	// Generalized second-price: each slot pays the MINIMUM bid that would hold its
	// rank — i.e. the price at which its relevance-weighted score just matches the
	// next-ranked product's score (score_{i+1} / relevance_i). The last filled slot
	// clears at the floor. Capped at the product's own bid (never pays more).
	floor := request.FloorPrice
	for i := 0; i < n; i++ {
		var nextScore float64
		if i+1 < len(ranked) {
			nextScore = ranked[i+1].score
		} else {
			nextScore = floor * ranked[i].rel // no next contender → floor
		}
		price := ranked[i].bid.Price
		if ranked[i].rel > 0 {
			if gsp := nextScore / ranked[i].rel; gsp < price {
				price = gsp
			}
		}
		if price < floor {
			price = floor
		}
		result.Winners = append(result.Winners, Winner{
			Bid:           ranked[i].bid,
			ClearingPrice: price,
			Position:      i + 1, // 1 = top sponsored slot
			TraceID:       request.TraceID,
		})
	}
	for i := n; i < len(ranked); i++ {
		result.LossBids = append(result.LossBids, LossBid{
			Bid:        ranked[i].bid,
			Reason:     LossOutbid,
			ReasonText: "outranked by relevance-weighted score",
		})
	}
	if slots > n {
		result.ShortFill = slots - n // fewer eligible products than sponsored slots
	}
	return result, nil
}

// retailRelevance scores how relevant a sponsored product (bid) is to what the
// shopper is looking at. Returns a 0..1 multiplier applied to the bid price.
func retailRelevance(request AuctionRequest, b Bid) float64 {
	if b.Relevance > 0 {
		if b.Relevance > 1 {
			return 1
		}
		return b.Relevance
	}
	// No browse/search signal → relevance-neutral (ranking becomes pure price).
	if len(request.RetailCategories) == 0 {
		return 1.0
	}
	if b.Category != "" {
		for _, c := range request.RetailCategories {
			if strings.EqualFold(strings.TrimSpace(c), strings.TrimSpace(b.Category)) {
				return 1.0
			}
		}
	}
	return relevanceMiss
}
