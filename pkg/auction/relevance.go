package auction

import "context"

// RelevanceWeightedStrategy selects multiple winners ranked by relevance * bid.
// A highly relevant product with a lower bid can beat an irrelevant product
// with a higher bid.
//
// Used for: retail media sponsored products.
//
// TODO(phase9): Implement when retail media channel is built.
// See docs/PLAN.md -> "Retail Media" and "Unified Auction Engine".
type RelevanceWeightedStrategy struct{}

func (s *RelevanceWeightedStrategy) Type() string { return "relevance_weighted" }

func (s *RelevanceWeightedStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	return Result{}, ErrNotImplemented
}
