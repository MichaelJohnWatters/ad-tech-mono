// Package publisheradserver implements publisher-side ad serving:
// direct-sold line items, arbitration against programmatic demand, and
// pacing for guaranteed-delivery commitments.
//
// The cmd/publisher-adserver service consumes this package on every
// inbound ad request. It first asks the arbitration package whether a
// direct-sold line item should win the impression; only if no direct
// line item wins does it fall through to programmatic (SSP→exchange→DSPs).
//
// See docs/PLAN.md → "Publisher-Side Ad Server (GAM-shaped features)"
// for the full arbitration ladder and design rationale.
package publisheradserver

import "time"

// PublisherLineItem is the publisher-owned direct-sold commitment. Mirrors
// the publisher_line_items table (migration 026). Distinct from
// models.LineItem, which is the advertiser-side bid configuration.
type PublisherLineItem struct {
	ID                   string
	AccountID            string
	PublisherID          string
	Name                 string
	DemandSource         string
	PriorityTier         string // sponsorship | guaranteed | preferred | house
	PlacementIDs         []string
	ImpressionsCommitted int64
	DeliveryStart        *time.Time
	DeliveryEnd          *time.Time
	CPM                  float64
	Currency             string
	PacingMode           string // even | asap
	Status               string // draft | active | paused | ended
	CreativeIDs          []string
}

// AppliesToPlacement reports whether this line item is eligible for the
// given placement. Empty PlacementIDs means "any placement under this
// publisher" — the publisher-level check happens before this is called.
func (li PublisherLineItem) AppliesToPlacement(placementID string) bool {
	if len(li.PlacementIDs) == 0 {
		return true
	}
	for _, id := range li.PlacementIDs {
		if id == placementID {
			return true
		}
	}
	return false
}

// WithinFlight reports whether now falls within the line item's delivery
// window. nil bounds are treated as open-ended on that side (a sponsorship
// with no end date runs indefinitely).
func (li PublisherLineItem) WithinFlight(now time.Time) bool {
	if li.DeliveryStart != nil && now.Before(*li.DeliveryStart) {
		return false
	}
	if li.DeliveryEnd != nil && now.After(*li.DeliveryEnd) {
		return false
	}
	return true
}

// Priority tier constants. Order matters: lower number = higher priority.
// The arbitration ladder evaluates tiers in this sequence.
const (
	TierSponsorship = "sponsorship"
	TierGuaranteed  = "guaranteed"
	TierPreferred   = "preferred"
	TierHouse       = "house"
)

// PacingMode constants.
const (
	PacingEven = "even"
	PacingASAP = "asap"
)

// Status constants.
const (
	StatusDraft  = "draft"
	StatusActive = "active"
	StatusPaused = "paused"
	StatusEnded  = "ended"
)
