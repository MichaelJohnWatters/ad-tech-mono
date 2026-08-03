//go:build e2e

// product_category via the PATCH path: setting it through the campaign management
// API (not just create) persists and drives retail relevance — proving the edit
// path, not only create.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetailProductCategoryViaPatch(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "prodcat-patch")

	// "shoes" content-targets business (IAB13); PATCH its product_category to
	// footwear so it ranks as a shoe. "loans" is a real business product.
	shoes := h.CreateCampaign(t, w.AdvAcc, w.IO, "e2e-pcp-shoes", 2.00, 500, "e2e-pcp-cr-shoes", "shoes.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}, Categories: []string{"IAB13"}})
	loans := h.CreateCampaign(t, w.AdvAcc, w.IO, "e2e-pcp-loans", 5.00, 500, "e2e-pcp-cr-loans", "loans.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}})
	h.PatchProductCategory(t, shoes, "IAB18-5")
	h.PatchProductCategory(t, loans, "IAB13")
	h.RefreshAllCaches(t)

	win := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Channel: "retail", Categories: "IAB18-5",
		Geo: "GBR", Device: "mobile",
	}))
	if win.NoBid || win.CampaignID != shoes.ID {
		which := "no fill"
		if win.CampaignID == loans.ID {
			which = "loans — PATCH product_category didn't take"
		}
		t.Fatalf("footwear browse went to %s, want 'shoes' via PATCHed product_category (%s)", which, shoes.ID)
	}
}
