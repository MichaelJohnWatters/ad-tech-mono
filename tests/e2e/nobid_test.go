//go:build e2e

// NoBid edge cases. The bid path must return cleanly (NoBid: true) and
// publish AuctionComplete when nobody bids — silent failures here hide
// real availability issues.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestNoBidWhenNoEligibleCampaigns — placement exists, advertiser has a
// campaign, but it targets a geo nobody is requesting from. Every DSP
// no-bids → exchange returns NoBid.
func TestNoBidWhenNoEligibleCampaigns(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "nobid-geo")

	// Force the campaign to target JPN only; auction asks for GBR.
	if _, err := h.DB.Exec(`UPDATE targeting_rules SET include_geo = $1, updated_at = now() WHERE line_item_id = $2`,
		"{JPN}", w.Campaign.ID); err != nil {
		t.Fatalf("update targeting: %v", err)
	}
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "nobid-geo-user")
	win := h.ExtractWinner(t, res)
	if !win.NoBid {
		t.Errorf("expected NoBid when no campaign targets requesting geo; got winner %q at %.4f",
			win.Seat, win.Price)
	}
}

// TestNoBidWhenAllBelowFloor — campaign exists and targeting matches,
// but the placement floor is higher than the campaign's base bid. The
// DSP submits, exchange drops the bid for being below floor → NoBid.
func TestNoBidWhenAllBelowFloor(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "nobid-floor")

	// Raise the placement floor to well above the 3.50 base bid.
	if _, err := h.DB.Exec(`UPDATE placements SET floor_price = $1, updated_at = now() WHERE id = $2`,
		50.00, w.Placement.ID); err != nil {
		t.Fatalf("update floor: %v", err)
	}
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "nobid-floor-user")
	win := h.ExtractWinner(t, res)
	if !win.NoBid {
		t.Errorf("expected NoBid when all bids below floor; got winner %q at %.4f",
			win.Seat, win.Price)
	}
}
