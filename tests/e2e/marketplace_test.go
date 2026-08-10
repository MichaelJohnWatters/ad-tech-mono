//go:build e2e

// Data Marketplace slice 1 — listings + discovery. A data owner lists a PUBLIC
// segment; another tenant sees it in the cross-tenant catalog with the seller's
// name and reach; the owner sees it only under scope=mine; a dsp_private segment
// can't be listed.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestMarketplaceListingAndDiscovery(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	uniq := time.Now().UnixNano()
	seller := h.CreateAdvertiser(t, fmt.Sprintf("mkt-seller-%d", uniq))
	buyer := h.CreateAdvertiser(t, fmt.Sprintf("mkt-buyer-%d", uniq))
	listingName := fmt.Sprintf("Sports Fans %d", uniq)

	segID := h.CreatePublicSegment(t, seller.ID, "e2e-sports", []string{"u1", "u2", "u3"})

	// Seller lists the public segment.
	status, body := h.MarketplaceList(t, seller.ID, segID, listingName, 0.50)
	if status != 200 {
		t.Fatalf("list status %d: %s", status, body)
	}

	// The seller sees it under scope=mine (reach = member count, price in micros).
	mine := h.MarketplaceCatalog(t, seller.ID, "mine")
	if !harness.HasListing(mine, listingName) {
		t.Fatalf("seller's own listing missing from scope=mine: %+v", mine)
	}
	for _, l := range mine {
		if l.Name == listingName {
			if l.SizeEstimate != 3 {
				t.Errorf("listing reach = %d, want 3 (member count)", l.SizeEstimate)
			}
			if l.CPMSurchargeMicros != 500_000 {
				t.Errorf("listing surcharge = %d micros, want 500000 ($0.50)", l.CPMSurchargeMicros)
			}
		}
	}

	// The BUYER (different tenant) sees it in the catalog with the seller's name.
	catalog := h.MarketplaceCatalog(t, buyer.ID, "")
	if !harness.HasListing(catalog, listingName) {
		t.Fatalf("buyer's catalog is missing the listing (cross-tenant browse): %+v", catalog)
	}
	for _, l := range catalog {
		if l.Name == listingName && l.SellerName == "" {
			t.Errorf("catalog listing has no seller_name")
		}
	}

	// The seller does NOT see their own listing in the browse catalog.
	sellerCatalog := h.MarketplaceCatalog(t, seller.ID, "")
	if harness.HasListing(sellerCatalog, listingName) {
		t.Errorf("seller sees their OWN listing in the browse catalog (should be excluded)")
	}

	// A dsp_private segment cannot be listed.
	var privSeg string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility)
VALUES ($1::uuid, 'e2e-private', 'first_party', 'active', 'seed', 'dsp_private') RETURNING id::text`,
		seller.ID).Scan(&privSeg); err != nil {
		t.Fatalf("create private segment: %v", err)
	}
	status, body = h.MarketplaceList(t, seller.ID, privSeg, "should fail", 1.0)
	if status != 400 || !strings.Contains(body, "PUBLIC") {
		t.Fatalf("listing a dsp_private segment should 400 with a PUBLIC hint, got %d: %s", status, body)
	}
}
