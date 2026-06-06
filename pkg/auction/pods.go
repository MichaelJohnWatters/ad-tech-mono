package auction

import (
	"context"
	"sort"
)

// PodStrategy fills an ad-break pod with one or more sequenced winners
// subject to duration + competitive-separation constraints.
//
// The real-world objective is to maximise revenue per pod subject to
// the constraints; the optimal solution is a small ILP. In practice
// every production ad-tech vendor approximates with a greedy by
// price-per-second pass — fast, near-optimal in the typical case, easy
// to reason about. We match the convention.
//
// Two pod shapes:
//
//   - Variable-duration (request.Pod.RqdDurs == nil): bids are sorted
//     CPM/sec descending, walked, and added when they fit the remaining
//     pod time and satisfy separation constraints.
//
//   - Fixed-slot (request.Pod.RqdDurs != nil): each slot has a required
//     duration; we pick the highest-CPM bid whose Duration matches that
//     slot's RqdDurs entry and that satisfies separation against
//     already-placed slots. A RqdDurs entry of 0 matches any duration
//     (rare; mostly to keep the loop uniform).
//
// Constraints honoured:
//   - request.Pod.MaxDuration   — cap on total pod time
//   - request.Pod.MaxAds        — cap on slot count (0 = unlimited)
//   - request.Pod.MinCPMPerSec  — per-second floor
//   - request.Pod.NoSameAdvertiserAdjacent
//   - request.Pod.UniqueAdvertiser
//   - request.Pod.UniqueCategory
//
// Result.ShortFill is the number of seconds (variable-duration) or
// slots (fixed-slot) we could not fill. Lets the exchange decide
// whether to emit slate / house ads for the gap.
type PodStrategy struct{}

func (s *PodStrategy) Type() string { return "pod" }

func (s *PodStrategy) Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	if len(bids) == 0 {
		return Result{StrategyType: s.Type()}, ErrNoBids
	}
	if request.Pod == nil {
		// Defensive: pod strategy invoked without pod context. Fall
		// back to single-winner semantics so a misrouted request still
		// gets SOMETHING back instead of a confusing empty result.
		return (&SingleWinnerStrategy{}).Select(ctx, bids, request)
	}

	if len(request.Pod.RqdDurs) > 0 {
		return fillFixedSlotPod(bids, request)
	}
	return fillVariablePod(bids, request)
}

// fillVariablePod implements the "free-form" pod fill: any combo of
// bids whose total duration fits in Pod.MaxDuration. Greedy by
// CPM/sec descending; matches what real ad servers do.
func fillVariablePod(bids []Bid, request AuctionRequest) (Result, error) {
	sorted := sortByCPMPerSec(bids)
	maxDur := request.Pod.MaxDuration
	maxAds := request.Pod.MaxAds
	remaining := maxDur

	state := newPodState(request)
	used := make(map[string]bool, len(sorted))
	var winners []Winner
	var losses []LossBid

	for _, bid := range sorted {
		key := bidKey(bid)
		if used[key] {
			continue
		}
		if bid.Duration <= 0 {
			losses = append(losses, LossBid{Bid: bid, Reason: LossInvalidBid, ReasonText: "missing or non-positive duration"})
			used[key] = true
			continue
		}
		if bid.Duration > remaining {
			// Doesn't fit. Don't drop yet — a later cheaper bid might
			// still fit. Unplaced bids fall through to the leftover
			// loop below.
			continue
		}
		if !state.passesSeparation(bid) {
			losses = append(losses, LossBid{Bid: bid, Reason: LossBlocked, ReasonText: "competitive separation"})
			used[key] = true
			continue
		}
		if maxAds > 0 && len(winners) >= maxAds {
			break
		}
		winners = append(winners, Winner{
			Bid:           bid,
			ClearingPrice: bid.Price,
			Position:      len(winners) + 1,
			TraceID:       request.TraceID,
		})
		state.record(bid)
		used[key] = true
		remaining -= bid.Duration
		if remaining <= 0 {
			break
		}
	}

	for _, bid := range sorted {
		if !used[bidKey(bid)] {
			losses = append(losses, LossBid{Bid: bid, Reason: LossOutbid, ReasonText: "did not fit pod"})
		}
	}

	if len(winners) == 0 {
		return Result{StrategyType: "pod", LossBids: losses}, ErrNoBids
	}
	return Result{
		Winners:       winners,
		LossBids:      losses,
		ShortFill:     remaining,
		IsMultiWinner: true,
		StrategyType:  "pod",
	}, nil
}

// fillFixedSlotPod handles the SSAI / pre-allocated case where each
// slot has a fixed required duration. We pick the best-CPM bid whose
// Duration matches the slot's RqdDurs entry, walking slot-by-slot.
func fillFixedSlotPod(bids []Bid, request AuctionRequest) (Result, error) {
	sorted := sortByCPMPerSec(bids)
	state := newPodState(request)
	used := make(map[string]bool, len(sorted))
	var winners []Winner
	var losses []LossBid
	shortSlots := 0

	for slot, reqDur := range request.Pod.RqdDurs {
		picked := false
		for _, bid := range sorted {
			if used[bidKey(bid)] {
				continue
			}
			if reqDur > 0 && bid.Duration != reqDur {
				continue
			}
			if bid.Duration <= 0 {
				continue
			}
			if !state.passesSeparation(bid) {
				continue
			}
			winners = append(winners, Winner{
				Bid:           bid,
				ClearingPrice: bid.Price,
				Position:      slot + 1,
				TraceID:       request.TraceID,
			})
			state.record(bid)
			used[bidKey(bid)] = true
			picked = true
			break
		}
		if !picked {
			shortSlots++
		}
	}

	for _, bid := range sorted {
		if !used[bidKey(bid)] {
			losses = append(losses, LossBid{Bid: bid, Reason: LossOutbid, ReasonText: "no matching slot"})
		}
	}

	if len(winners) == 0 {
		return Result{StrategyType: "pod", LossBids: losses}, ErrNoBids
	}
	return Result{
		Winners:       winners,
		LossBids:      losses,
		ShortFill:     shortSlots,
		IsMultiWinner: true,
		StrategyType:  "pod",
	}, nil
}

// podState tracks placement context so the separation predicates can
// decide whether a candidate bid is allowed in the next slot.
type podState struct {
	req       *PodRequest
	lastAdvID string
	usedAdvID map[string]bool
	usedCat   map[string]bool
}

func newPodState(request AuctionRequest) *podState {
	return &podState{
		req:       request.Pod,
		usedAdvID: make(map[string]bool),
		usedCat:   make(map[string]bool),
	}
}

func (s *podState) passesSeparation(bid Bid) bool {
	if s.req == nil {
		return true
	}
	if s.req.NoSameAdvertiserAdjacent && bid.AdvertiserID != "" && bid.AdvertiserID == s.lastAdvID {
		return false
	}
	if s.req.UniqueAdvertiser && bid.AdvertiserID != "" && s.usedAdvID[bid.AdvertiserID] {
		return false
	}
	if s.req.UniqueCategory && bid.Category != "" && s.usedCat[bid.Category] {
		return false
	}
	if s.req.MinCPMPerSec > 0 && bid.Duration > 0 {
		// Bid.Price is per-impression CPM. CPM/sec = Price / Duration.
		// Comparing both sides in CPM/sec keeps the unit consistent.
		if bid.Price/float64(bid.Duration) < s.req.MinCPMPerSec {
			return false
		}
	}
	return true
}

func (s *podState) record(bid Bid) {
	s.lastAdvID = bid.AdvertiserID
	if bid.AdvertiserID != "" {
		s.usedAdvID[bid.AdvertiserID] = true
	}
	if bid.Category != "" {
		s.usedCat[bid.Category] = true
	}
}

// sortByCPMPerSec orders bids by price-per-second descending. Bids
// with zero or negative duration sort last — they can't fit usefully
// but we still want them surfaced as LossInvalidBid downstream.
func sortByCPMPerSec(bids []Bid) []Bid {
	out := make([]Bid, len(bids))
	copy(out, bids)
	sort.SliceStable(out, func(i, j int) bool {
		ai := cpmPerSec(out[i])
		aj := cpmPerSec(out[j])
		if ai == aj {
			return out[i].Price > out[j].Price
		}
		return ai > aj
	})
	return out
}

func cpmPerSec(b Bid) float64 {
	if b.Duration <= 0 {
		return -1
	}
	return b.Price / float64(b.Duration)
}

// bidKey is a stable identifier for dedup tracking. DSPID+CampaignID+
// CreativeID is the same composite the rest of the auction engine
// uses (filterLossBids in strategy.go keys by DSPID+CampaignID —
// CreativeID is added here so a campaign with multi-size creatives
// doesn't double-count in the pod).
func bidKey(b Bid) string {
	return b.DSPID + "|" + b.CampaignID + "|" + b.CreativeID
}
