package auction

import "context"

// BatchStrategy selects multiple winners for multiple placements in one request.
// Assigns bids to placements with cross-placement competitive separation.
//
// Used for: in-game intrinsic billboards (fill N billboards in one scene).
//
// TODO(phase9): Implement when in-game channel is built.
// See docs/PLAN.md -> "In-Game Advertising" and "Unified Auction Engine".
type BatchStrategy struct{}

func (s *BatchStrategy) Type() string { return "batch" }

func (s *BatchStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	return Result{}, ErrNotImplemented
}
