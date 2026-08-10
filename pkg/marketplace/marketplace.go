// Package marketplace is the data marketplace (PLAN Phase 10): a storefront
// where a data owner lists a PUBLIC audience segment for other tenants to
// discover and (later slices) purchase targeting access to. A listing exposes
// only aggregate characteristics — never individual segment members. Builds on
// the shipped data-monetization foundation (audience_segments visibility +
// data_fee, data_providers, the data-fee settlement ledger).
package marketplace

import (
	"context"
	"encoding/json"
	"time"
)

// Listing statuses.
const (
	StatusActive    = "active"
	StatusPaused    = "paused"
	StatusWithdrawn = "withdrawn"
)

// IsValidStatus reports whether s is a listing status.
func IsValidStatus(s string) bool {
	switch s {
	case StatusActive, StatusPaused, StatusWithdrawn:
		return true
	}
	return false
}

// Listing is one marketplace storefront entry for a segment.
type Listing struct {
	ID                 string          `json:"id"`
	AccountID          string          `json:"account_id"`
	SegmentID          string          `json:"segment_id"`
	Name               string          `json:"name"`
	Description        string          `json:"description,omitempty"`
	SizeEstimate       int64           `json:"size_estimate"`
	CPMSurchargeMicros int64           `json:"cpm_surcharge_micros"`
	Preview            json.RawMessage `json:"preview,omitempty"`
	Status             string          `json:"status"`
	// SellerName is the owner account's display name — populated on the catalog
	// browse so a buyer sees WHO is selling (never on the owner's own list).
	SellerName string    `json:"seller_name,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CPMSurchargeUSD renders the surcharge as dollars for display.
func (l Listing) CPMSurchargeUSD() float64 { return float64(l.CPMSurchargeMicros) / 1e6 }

// Store is the marketplace persistence seam. Owner-facing writes/reads are
// tenant-scoped (RLS GUC); the catalog browse is cross-tenant (platform hatch).
type Store interface {
	// UpsertListing creates-or-updates the account's listing for a segment
	// (keyed on segment_id). Returns the listing id. The caller must have
	// verified the segment is the account's own PUBLIC segment.
	UpsertListing(ctx context.Context, l Listing) (string, error)
	// ListByAccount returns the account's own listings (seller view).
	ListByAccount(ctx context.Context, accountID string, limit int) ([]Listing, error)
	// Catalog returns ACTIVE listings across all tenants (buyer browse),
	// excluding the caller's own, with the seller's display name. Cross-tenant
	// read via the platform hatch.
	Catalog(ctx context.Context, excludeAccountID string, limit int) ([]Listing, error)
	// GetByID returns one active listing by id (cross-tenant, for purchase/
	// estimate flows). nil when absent or not active.
	GetByID(ctx context.Context, id string) (*Listing, error)
	// SetStatus pauses/withdraws/reactivates the account's own listing.
	SetStatus(ctx context.Context, accountID, id, status string) error
}
