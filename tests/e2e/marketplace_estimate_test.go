//go:build e2e

// Data Marketplace slice 4 — expansion estimates (clean-room-lite). A buyer
// estimates the overlap + incremental reach of a listing against their own
// audience BEFORE purchasing. Aggregate-only, real set intersection, with a
// min-aggregation privacy floor that suppresses a small overlap.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestMarketplaceExpansionEstimate(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	uniq := time.Now().UnixNano()
	seller := h.CreateAdvertiser(t, fmt.Sprintf("mkt-e-seller-%d", uniq))
	buyer := h.CreateAdvertiser(t, fmt.Sprintf("mkt-e-buyer-%d", uniq))

	// Listing segment: 200 users (shared s0..s199).
	shared := make([]string, 200)
	for i := range shared {
		shared[i] = fmt.Sprintf("est-s-%d-%d", uniq, i)
	}
	sellerSeg := h.CreatePublicSegment(t, seller.ID, "e2e-est-listing", shared)
	if st, body := h.MarketplaceList(t, seller.ID, sellerSeg, fmt.Sprintf("Est Listing %d", uniq), 0.60); st != 200 {
		t.Fatalf("list: %d %s", st, body)
	}
	listingID := h.FindListingID(t, buyer.ID, fmt.Sprintf("Est Listing %d", uniq))
	if listingID == "" {
		t.Fatal("listing not found in catalog")
	}

	// Buyer audience A: first 120 shared users + 30 of their own → overlap 120,
	// above the 100 floor (NOT suppressed). size 150.
	audA := append(append([]string{}, shared[:120]...), genIDs(fmt.Sprintf("est-a-%d", uniq), 30)...)
	segA := h.CreatePublicSegment(t, buyer.ID, "e2e-est-buyerA", audA)

	e := h.MarketplaceEstimateFor(t, buyer.ID, listingID, segA)
	if e.OverlapSuppressed {
		t.Fatalf("overlap 120 should NOT be suppressed (floor %d): %+v", e.MinAggregation, e)
	}
	if e.Overlap == nil || *e.Overlap != 120 {
		t.Fatalf("overlap = %v, want 120", e.Overlap)
	}
	if e.YourAudienceSize != 150 || e.ListingSize != 200 {
		t.Errorf("sizes = your %d / listing %d, want 150 / 200", e.YourAudienceSize, e.ListingSize)
	}
	if e.NewReachableUsers != 80 { // 200 listing - 120 overlap
		t.Errorf("new_reachable = %d, want 80", e.NewReachableUsers)
	}
	// expansion_factor = (150 + 80) / 150 ≈ 1.533
	if e.ExpansionFactor < 1.5 || e.ExpansionFactor > 1.56 {
		t.Errorf("expansion_factor = %v, want ~1.53", e.ExpansionFactor)
	}

	// Buyer audience B: only 40 shared users + 60 own → overlap 40, BELOW the
	// floor → suppressed (exact overlap withheld).
	audB := append(append([]string{}, shared[:40]...), genIDs(fmt.Sprintf("est-b-%d", uniq), 60)...)
	segB := h.CreatePublicSegment(t, buyer.ID, "e2e-est-buyerB", audB)

	e2 := h.MarketplaceEstimateFor(t, buyer.ID, listingID, segB)
	if !e2.OverlapSuppressed {
		t.Fatalf("overlap 40 should be suppressed (floor %d): %+v", e2.MinAggregation, e2)
	}
	if e2.Overlap != nil {
		t.Errorf("suppressed estimate must not reveal the exact overlap: %v", *e2.Overlap)
	}
	// Conservative: new_reachable computed with overlap=0 → the full listing.
	if e2.NewReachableUsers != 200 {
		t.Errorf("suppressed new_reachable = %d, want 200 (conservative, overlap hidden)", e2.NewReachableUsers)
	}
}

func genIDs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return out
}
