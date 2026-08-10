//go:build e2e

// Data Marketplace slice 2 — purchase + activation. A buyer purchases access to
// a listing → a grant (their right to target the seller's segment). The buyer
// sees it under their purchased data; the seller sees the sale; you can't buy
// your own listing; re-purchase renews. Because the listed segment is PUBLIC it
// already rides every bid request, so the granted segment is immediately
// targetable with no bid-path change.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestMarketplacePurchaseAndGrant(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	uniq := time.Now().UnixNano()
	seller := h.CreateAdvertiser(t, fmt.Sprintf("mkt-p-seller-%d", uniq))
	buyer := h.CreateAdvertiser(t, fmt.Sprintf("mkt-p-buyer-%d", uniq))
	listingName := fmt.Sprintf("Auto Intenders %d", uniq)

	segID := h.CreatePublicSegment(t, seller.ID, "e2e-auto", []string{"a1", "a2"})
	if st, body := h.MarketplaceList(t, seller.ID, segID, listingName, 0.40); st != 200 {
		t.Fatalf("list status %d: %s", st, body)
	}
	listingID := h.FindListingID(t, buyer.ID, listingName)
	if listingID == "" {
		t.Fatalf("buyer can't find the listing in the catalog")
	}

	// Buyer purchases.
	st, body := h.MarketplacePurchase(t, buyer.ID, listingID)
	if st != 200 {
		t.Fatalf("purchase status %d: %s", st, body)
	}

	// Buyer's purchased data shows the grant with the right segment + price + names.
	grants := h.MarketplaceGrants(t, buyer.ID, "")
	if len(grants) != 1 {
		t.Fatalf("buyer has %d grants, want 1", len(grants))
	}
	g := grants[0]
	if g.SegmentID != segID {
		t.Errorf("grant segment = %s, want %s (the seller's segment)", g.SegmentID, segID)
	}
	if g.CPMSurchargeMicros != 400_000 {
		t.Errorf("grant surcharge = %d micros, want 400000 ($0.40)", g.CPMSurchargeMicros)
	}
	if g.ListingName != listingName || g.SellerName == "" {
		t.Errorf("grant missing listing/seller name: %+v", g)
	}
	if g.Status != "active" {
		t.Errorf("grant status = %q, want active", g.Status)
	}

	// Seller's sales view shows the buyer.
	sales := h.MarketplaceGrants(t, seller.ID, "sales")
	if len(sales) != 1 || sales[0].BuyerAccountID != buyer.ID {
		t.Fatalf("seller's sales = %+v, want 1 grant to the buyer", sales)
	}

	// You cannot buy your own listing.
	if st, body := h.MarketplacePurchase(t, seller.ID, listingID); st != 400 || !strings.Contains(body, "own listing") {
		t.Fatalf("buying own listing should 400, got %d: %s", st, body)
	}

	// Re-purchase renews (idempotent — still one grant).
	if st, body := h.MarketplacePurchase(t, buyer.ID, listingID); st != 200 {
		t.Fatalf("re-purchase status %d: %s", st, body)
	}
	if again := h.MarketplaceGrants(t, buyer.ID, ""); len(again) != 1 {
		t.Fatalf("after re-purchase buyer has %d grants, want 1 (renew, not duplicate)", len(again))
	}
}
