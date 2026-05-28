package auction

import "context"

// PodStrategy selects multiple winners to fill an ad break duration.
// Bin-packing with competitive separation constraints.
//
// Used for: video ad pods, audio ad breaks.
//
// TODO(phase9): Implement when video/audio channels are built.
// See docs/PLAN.md -> "Ad Pods" and "Video Ads, SSAI, and CTV".
type PodStrategy struct{}

func (s *PodStrategy) Type() string { return "pod" }

func (s *PodStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	return Result{}, ErrNotImplemented
}
