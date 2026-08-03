package auction

import "context"

// TimeSlotStrategy fills the next rotation slot on a DOOH screen.
//
// A DOOH screen (billboard, transit panel) shows one ad per play and re-requests
// for each slot in its loop cycle, so a single request is a single-winner auction:
// the highest bid takes the slot and pays the clearing price per the price mode
// (delegated to SingleWinnerStrategy). The DOOH-distinctive part — one play counts
// as N impressions (the venue's estimated audience) — is applied downstream at
// proof-of-play (tracker imp `mult`), not in the auction, so the money is priced
// per delivered audience while the auction stays a clean per-slot contest.
//
// (Filling a whole loop's worth of slots in one call — return the top-N — is a
// future refinement; the per-slot model is how a real player pulls DOOH creatives.)
//
// See docs/PLAN.md -> "Digital Out-of-Home (DOOH)".
type TimeSlotStrategy struct{}

func (s *TimeSlotStrategy) Type() string { return "timeslot" }

func (s *TimeSlotStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	return (&SingleWinnerStrategy{}).Select(ctx, bids, request)
}
