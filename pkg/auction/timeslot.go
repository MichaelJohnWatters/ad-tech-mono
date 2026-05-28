package auction

import "context"

// TimeSlotStrategy fills rotation slots on a DOOH screen.
// Each winner gets a time slot in the screen's rotation cycle.
//
// Used for: digital out-of-home (billboards, transit screens).
//
// TODO(phase9): Implement when DOOH channel is built.
// See docs/PLAN.md -> "Digital Out-of-Home (DOOH)" and "Unified Auction Engine".
type TimeSlotStrategy struct{}

func (s *TimeSlotStrategy) Type() string { return "timeslot" }

func (s *TimeSlotStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	return Result{}, ErrNotImplemented
}
