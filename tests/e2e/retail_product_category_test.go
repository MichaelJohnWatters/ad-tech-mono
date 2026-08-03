//go:build e2e

// Per-product category: retail relevance scores a sponsored product against the
// shopper's browsed categories using the product's OWN category (line_items.
// product_category), NOT its content-targeting include_categories. Proves a
// shoe product whose CONTENT targeting is business (IAB13) but whose PRODUCT is
// footwear (IAB18-5) wins a shoe-browse over a real business product bidding more.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetailProductCategoryDrivesRelevance(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "retail-prodcat")

	// "shoes": content-targets business pages (include_categories IAB13) but the
	// PRODUCT is footwear — product_category IAB18-5. Bids low.
	shoes := h.CreateCampaign(t, w.AdvAcc, w.IO, "e2e-pc-shoes", 2.00, 500, "e2e-pc-cr-shoes", "shoes.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}, Categories: []string{"IAB13"}})
	// "loans": a real business product (product_category IAB13), bids higher.
	loans := h.CreateCampaign(t, w.AdvAcc, w.IO, "e2e-pc-loans", 5.00, 500, "e2e-pc-cr-loans", "loans.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}})
	setProductCategory(t, h, shoes.ID, "IAB18-5")
	setProductCategory(t, h, loans.ID, "IAB13")
	h.RefreshAllCaches(t)

	// Shopper browsing footwear (IAB18-5).
	win := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Channel: "retail", Categories: "IAB18-5",
		Geo: "GBR", Device: "mobile",
	}))
	if win.NoBid {
		t.Fatal("retail request did not fill")
	}
	// shoes wins on its PRODUCT category (IAB18-5), despite its content targeting
	// being IAB13 and loans bidding more.
	if win.CampaignID != shoes.ID {
		which := "an unexpected campaign"
		if win.CampaignID == loans.ID {
			which = "loans — relevance used content targeting, not product_category"
		}
		t.Fatalf("slot went to %s, want 'shoes' by product_category (%s)", which, shoes.ID)
	}
}

func setProductCategory(t *testing.T, h *harness.Harness, campaignID, cat string) {
	t.Helper()
	if _, err := h.DB.Exec(`UPDATE line_items SET product_category = $1 WHERE id = $2`, cat, campaignID); err != nil {
		t.Fatalf("set product_category: %v", err)
	}
}
