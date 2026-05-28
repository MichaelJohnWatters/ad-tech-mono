// Package auction - competitive separation logic.
//
// Prevents competitor ads from appearing together on the same page,
// in the same ad pod, or in the same screen rotation.
package auction

// SeparationContext tracks which advertisers/categories have already
// won on the current page (for per-page competitive separation).
type SeparationContext struct {
	advertisers map[string]bool
	categories  map[string]bool
}

// NewSeparationContext creates an empty separation context for a page.
func NewSeparationContext() *SeparationContext {
	return &SeparationContext{
		advertisers: make(map[string]bool),
		categories:  make(map[string]bool),
	}
}

// RecordWinner adds a winner to the separation context.
func (sc *SeparationContext) RecordWinner(advertiserID, category string) {
	sc.advertisers[advertiserID] = true
	if category != "" {
		sc.categories[category] = true
	}
}

// IsBlocked checks if a bid would violate competitive separation.
func (sc *SeparationContext) IsBlocked(advertiserID, category string) bool {
	// Self-separation: same advertiser already on this page
	if sc.advertisers[advertiserID] {
		return true
	}
	// Category separation: same category already on this page
	if category != "" && sc.categories[category] {
		return true
	}
	return false
}

// FilterBidsWithSeparation removes bids that violate competitive separation
// against the current page context.
func FilterBidsWithSeparation(bids []Bid, sc *SeparationContext) []Bid {
	if sc == nil {
		return bids
	}
	var eligible []Bid
	for _, bid := range bids {
		if !sc.IsBlocked(bid.AdvertiserID, bid.Category) {
			eligible = append(eligible, bid)
		}
	}
	return eligible
}
