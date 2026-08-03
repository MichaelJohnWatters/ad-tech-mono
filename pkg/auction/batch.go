package auction

import (
	"context"
	"sort"
)

// BatchStrategy fills every ad surface in ONE in-game scene from a single auction.
// An intrinsic in-game placement is a set of billboards/surfaces inside a 3D scene
// (a stadium's hoardings, a racetrack's signage), and each bid is "for any surface
// in this scene". The auction assigns the top bids to the SlotCount surfaces with
// cross-surface competitive separation: one advertiser per scene, and one category
// per scene — you don't want Coke on three hoardings, or Coke next to Pepsi in the
// same shot. Each winner clears first-price on its surface.
//
// Unfilled surfaces (fewer non-competing products than surfaces) fall to
// house/default ads downstream — surfaced here as ShortFill.
//
// See docs/PLAN.md -> "In-Game Advertising" and "Unified Auction Engine".
type BatchStrategy struct{}

func (s *BatchStrategy) Type() string { return "batch" }

func (s *BatchStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	if len(bids) == 0 {
		return Result{}, ErrNoBids
	}

	slots := request.SlotCount
	if slots < 1 {
		slots = 1
	}

	sorted := make([]Bid, len(bids))
	copy(sorted, bids)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Price > sorted[j].Price })

	seenAdv := map[string]bool{}
	seenCat := map[string]bool{}
	result := Result{IsMultiWinner: true}
	for _, b := range sorted {
		if len(result.Winners) >= slots {
			result.LossBids = append(result.LossBids, LossBid{Bid: b, Reason: LossOutbid, ReasonText: "all scene surfaces filled"})
			continue
		}
		// Competitive separation: one advertiser and one category per scene.
		if b.AdvertiserID != "" && seenAdv[b.AdvertiserID] {
			result.LossBids = append(result.LossBids, LossBid{Bid: b, Reason: LossBlocked, ReasonText: "advertiser already placed in scene (competitive separation)"})
			continue
		}
		if b.Category != "" && seenCat[b.Category] {
			result.LossBids = append(result.LossBids, LossBid{Bid: b, Reason: LossBlocked, ReasonText: "category already placed in scene (competitive separation)"})
			continue
		}
		result.Winners = append(result.Winners, Winner{
			Bid:           b,
			ClearingPrice: b.Price, // first-price per surface
			Position:      len(result.Winners) + 1,
			TraceID:       request.TraceID,
		})
		if b.AdvertiserID != "" {
			seenAdv[b.AdvertiserID] = true
		}
		if b.Category != "" {
			seenCat[b.Category] = true
		}
	}
	if slots > len(result.Winners) {
		result.ShortFill = slots - len(result.Winners) // surfaces left for house ads
	}
	return result, nil
}
