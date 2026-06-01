package deals

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"

// Decision is the per-bid output of the deal selection step. It tells the
// auction strategy which DealID (if any) the bid is participating in, what
// the effective floor is for this bid, and whether the bid should be considered
// preemptively (PG bids skip the open auction entirely).
type Decision struct {
	DealID         string  // empty = no deal, runs in open auction
	DealType       string  // one of TypePG / TypePreferred / TypePMP / TypeOpen
	EffectiveFloor float64 // max(placement floor, deal price)
	Preempt        bool    // true for PG — that bid wins automatically
}

// Decide picks the highest-priority deal a single (advertiser, bid) qualifies
// for given the placement context. eligibleDeals is the output of Matcher.Match
// for this advertiser. Returns an "open" decision with placement floor if
// no deal applies.
//
// PG short-circuits: the first PG match preempts the rest of the auction.
// Preferred and PMP set the floor + DealID but the strategy still picks the
// winner among matched bids (and outbids open bids when Preferred is involved).
func Decide(advertiserID string, placementFloor float64, eligibleDeals []models.Deal) Decision {
	for _, d := range eligibleDeals {
		switch d.DealType {
		case TypePG:
			return Decision{
				DealID: d.ID, DealType: TypePG,
				EffectiveFloor: maxF(placementFloor, d.Price),
				Preempt:        true,
			}
		case TypePreferred:
			return Decision{
				DealID: d.ID, DealType: TypePreferred,
				EffectiveFloor: maxF(placementFloor, d.Price),
			}
		case TypePMP:
			// PMP is gated to the allowlist — Matcher.Match already enforced
			// that this advertiser is allowed, so we honor the deal floor.
			return Decision{
				DealID: d.ID, DealType: TypePMP,
				EffectiveFloor: maxF(placementFloor, d.Price),
			}
		}
	}
	return Decision{DealType: TypeOpen, EffectiveFloor: placementFloor}
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
