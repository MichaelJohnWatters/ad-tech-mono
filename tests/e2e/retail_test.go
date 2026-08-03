//go:build e2e

// Retail-media end-to-end. The defining mechanic: sponsored products are ranked
// by relevance × bid, so a product that matches what the shopper is browsing wins
// the slot over a less-relevant product bidding MORE. This proves that ranking
// live through the real path — SSP builds a retail request (imp.ext.channel=retail
// + the browsed category on Site.Cat), the DSP returns its product SLATE, and the
// exchange's relevance-weighted strategy picks the relevant-but-cheaper product.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetailRelevanceBeatsHigherBid(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "retail")

	// Two sponsored products under the SAME advertiser, both eligible on geo/device.
	// "shoes" is what the shopper is browsing (IAB18-5) but bids LOW; "loans"
	// (IAB13) is off-category but bids nearly 3x higher. A display auction would
	// hand the slot to loans on price; retail must hand it to shoes on relevance.
	shoes := h.CreateCampaign(t, w.AdvAcc, w.IO, "e2e-retail-shoes", 2.00, 500,
		"e2e-cr-retail-shoes", "shoes.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}, Categories: []string{"IAB18-5"}})
	loans := h.CreateCampaign(t, w.AdvAcc, w.IO, "e2e-retail-loans", 5.50, 500,
		"e2e-cr-retail-loans", "loans.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}, Categories: []string{"IAB13"}})
	h.RefreshAllCaches(t)

	// Retail request: shopper browsing shoes (cat=IAB18-5).
	auc := h.RunAuctionWith(t, harness.AuctionParams{
		Placement:  w.Placement.ExternalID,
		Channel:    "retail",
		Categories: "IAB18-5",
		Geo:        "GBR",
		Device:     "mobile",
	})
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("retail request did not fill (both products should bid; slate ranked by relevance)")
	}

	// The relevant, CHEAPER product wins the sponsored slot despite the higher bid.
	if win.CampaignID != shoes.ID {
		which := "an unexpected campaign"
		if win.CampaignID == loans.ID {
			which = "loans (the higher bid) — relevance weighting did not apply"
		}
		t.Fatalf("retail slot went to %s, want the relevant 'shoes' product (%s)", which, shoes.ID)
	}
	// GSP: shoes pays only the minimum to hold rank 1 — loans' score (0.1×5.50=0.55)
	// / shoes' relevance (1.0) = 0.55, which is below the placement's 1.00 reserve,
	// so it clears at the reserve. Either way, far below its own 2.00 bid.
	if win.Price < 0.99 || win.Price > 1.01 {
		t.Errorf("clearing price = %.2f, want ~1.00 (GSP floored at the reserve)", win.Price)
	}
}
