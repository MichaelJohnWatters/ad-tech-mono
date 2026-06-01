// Package deals matches eligible deals to a bid request and orders them by
// the platform priority rules (Programmatic Guaranteed > Preferred > PMP > Open).
//
// The exchange holds a warm cache of active deals (warm.Cache[models.Deal]) and
// calls Matcher.Match per auction. The returned list is ordered so the first
// element, if any, is the highest-priority deal that any of the candidate
// bids could fill. PG short-circuits the auction entirely; Preferred ranks
// matched bids above open-auction bids; PMP enforces an advertiser allowlist
// and applies the deal floor.
package deals

import (
	"sort"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// Deal type constants — mirror the deal_type CHECK constraint on the deals table.
const (
	TypePG        = "pg"
	TypePreferred = "preferred"
	TypePMP       = "pmp"
	TypeOpen      = "open"
)

// Priority returns a sort key for a deal type — lower means earlier.
// PG (guaranteed) beats everything; Open is the implicit default with no deal.
func Priority(dealType string) int {
	switch dealType {
	case TypePG:
		return 0
	case TypePreferred:
		return 1
	case TypePMP:
		return 2
	case TypeOpen:
		return 3
	}
	return 99
}

// Request is the auction-side input to matching: the inventory being sold
// and the advertiser whose bid we're checking. AdvertiserID may be empty
// when listing all deals available on a placement (e.g. for the SSP to
// advertise deal IDs in the bid request); leave it empty to skip the
// advertiser allowlist filter.
type Request struct {
	PublisherID  string
	PlacementID  string
	AdvertiserID string
	Now          time.Time
}

// Matcher is constructed from the warm-cache snapshot of active deals.
// Construction is O(n); Match is O(n) per call — adequate for the typical
// hundreds-of-deals-per-publisher scale and keeps the data structure simple.
// For >10k deals we'd index by publisher_id, but we're nowhere near that.
type Matcher struct {
	deals []models.Deal
}

// New constructs a matcher from the warm cache snapshot. The slice is
// retained as-is — callers must not mutate it after passing in.
func New(deals []models.Deal) *Matcher {
	return &Matcher{deals: deals}
}

// Match returns every deal that applies to req, sorted by Priority. An empty
// result means the auction runs as Open. Time-window filtering uses req.Now
// so the caller controls the clock (tests pass a fake).
func (m *Matcher) Match(req Request) []models.Deal {
	var matched []models.Deal
	for _, d := range m.deals {
		if !eligible(d, req) {
			continue
		}
		matched = append(matched, d)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return Priority(matched[i].DealType) < Priority(matched[j].DealType)
	})
	return matched
}

// eligible applies the per-deal filters: same publisher, placement in
// (allowlist or empty=all), advertiser in (allowlist or empty=all), and
// the deal is active during req.Now.
func eligible(d models.Deal, req Request) bool {
	if d.Status != "active" {
		return false
	}
	if d.PublisherID != req.PublisherID {
		return false
	}
	if len(d.PlacementIDs) > 0 && !contains(d.PlacementIDs, req.PlacementID) {
		return false
	}
	if req.AdvertiserID != "" && len(d.AdvertiserIDs) > 0 && !contains(d.AdvertiserIDs, req.AdvertiserID) {
		return false
	}
	if d.StartDate != nil && req.Now.Before(*d.StartDate) {
		return false
	}
	if d.EndDate != nil && req.Now.After(*d.EndDate) {
		return false
	}
	return true
}

func contains(arr []string, s string) bool {
	for _, v := range arr {
		if v == s {
			return true
		}
	}
	return false
}
