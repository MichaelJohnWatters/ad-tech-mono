// Package arbitration implements the per-impression decision: which demand
// source serves this ad request? Evaluates direct-sold line items against
// programmatic in the priority order documented in docs/PLAN.md →
// "Publisher-Side Ad Server".
//
// The arbiter doesn't fetch line items or call programmatic itself — those
// are injected. It just runs the ladder over the eligible set and returns
// what the caller should do.
package arbitration

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver"
)

// Request captures everything the arbiter needs about an inbound ad
// request. The caller (cmd/publisher-adserver) populates it from the
// HTTP query string / inventory cache.
type Request struct {
	PublisherID string
	PlacementID string
	Now         time.Time
}

// DecisionType signals what the caller should do next.
type DecisionType int

const (
	// DecisionDirect — a direct-sold line item won. Caller should fetch
	// the line item's creative and serve it.
	DecisionDirect DecisionType = iota
	// DecisionProgrammatic — no direct line item won; fall through to
	// the programmatic auction (SSP → exchange → DSPs).
	DecisionProgrammatic
	// DecisionHouse — programmatic fell through too (or wasn't tried).
	// Serve a house ad. Recommended starting scope wraps house into the
	// same direct-tier ladder via TierHouse; this exists for future
	// expansion when programmatic-then-house becomes a distinct path.
	DecisionHouse
)

// Decision is what the arbiter returns. LineItem is set only when
// Type == DecisionDirect or DecisionHouse.
type Decision struct {
	Type     DecisionType
	LineItem *publisheradserver.PublisherLineItem
	// Reason is a short human-readable tag for logging / debugging.
	// Examples: "sponsorship-wins", "guaranteed-behind-pace",
	// "no-direct-line-items", "all-paused".
	Reason string
}

// PacingDecider is consulted for tiers that have delivery commitments
// (sponsorship, guaranteed). Returns true if the line item should serve
// this impression based on current pace.
//
// Implementations live in pkg/publisheradserver/pacing. Injected here so
// arbitration stays pure / unit-testable without Redis.
type PacingDecider interface {
	ShouldServe(li publisheradserver.PublisherLineItem, now time.Time) bool
}

// Decide runs the arbitration ladder over the eligible line items.
//
// Recommended starting scope (per PLAN.md) implements:
//   - sponsorship: always wins when active + in flight + applies to placement
//   - guaranteed: wins if behind pace (PacingDecider says so)
//   - preferred: not implemented in starting scope — falls through
//   - house: wins only if nothing above did (sourced from the same line
//     item set but with TierHouse; treated as the final direct option)
//
// The caller passes the full set of publisher line items (the warm cache
// snapshot). Arbiter filters by status, flight window, publisher/placement
// match. Programmatic fall-through is implicit: if no tier wins, the caller
// triggers the existing SSP/exchange/DSP path.
func Decide(req Request, all []publisheradserver.PublisherLineItem, pacing PacingDecider) Decision {
	eligible := filterEligible(req, all)
	if len(eligible) == 0 {
		return Decision{Type: DecisionProgrammatic, Reason: "no-direct-line-items"}
	}

	// Sponsorship: first one wins. Multiple active sponsorships on the
	// same placement is a publisher trafficking error; we pick the first
	// (stable order from the warm cache) rather than failing the request.
	for i := range eligible {
		if eligible[i].PriorityTier == publisheradserver.TierSponsorship {
			return Decision{
				Type:     DecisionDirect,
				LineItem: &eligible[i],
				Reason:   "sponsorship-wins",
			}
		}
	}

	// Guaranteed: highest priority among those behind pace. The pacing
	// decider answers "would serving this impression keep me on track?"
	// If multiple are behind pace, the first eligible one wins (rotation
	// across guarantees is a later refinement).
	for i := range eligible {
		if eligible[i].PriorityTier == publisheradserver.TierGuaranteed {
			if pacing == nil || pacing.ShouldServe(eligible[i], req.Now) {
				return Decision{
					Type:     DecisionDirect,
					LineItem: &eligible[i],
					Reason:   "guaranteed-behind-pace",
				}
			}
		}
	}

	// House is intentionally skipped in this pass. The PLAN ladder puts
	// programmatic ahead of house — caller falls through to programmatic
	// here, then calls DecideHouse only on no-bid.
	return Decision{Type: DecisionProgrammatic, Reason: "no-direct-winner"}
}

// DecideHouse is the fallback after programmatic returns no-bid. Returns
// a house line item if any matches the request, else DecisionHouse with a
// nil LineItem (caller decides whether to serve a default house creative
// or emit a passback).
func DecideHouse(req Request, all []publisheradserver.PublisherLineItem) Decision {
	eligible := filterEligible(req, all)
	for i := range eligible {
		if eligible[i].PriorityTier == publisheradserver.TierHouse {
			return Decision{
				Type:     DecisionDirect,
				LineItem: &eligible[i],
				Reason:   "house-fallback",
			}
		}
	}
	return Decision{Type: DecisionHouse, Reason: "no-house-available"}
}

func filterEligible(req Request, all []publisheradserver.PublisherLineItem) []publisheradserver.PublisherLineItem {
	out := make([]publisheradserver.PublisherLineItem, 0, len(all))
	for _, li := range all {
		if li.PublisherID != req.PublisherID {
			continue
		}
		if li.Status != publisheradserver.StatusActive {
			continue
		}
		if !li.WithinFlight(req.Now) {
			continue
		}
		if !li.AppliesToPlacement(req.PlacementID) {
			continue
		}
		out = append(out, li)
	}
	return out
}
