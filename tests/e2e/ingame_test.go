//go:build e2e

// In-game intrinsic end-to-end. An intrinsic placement is a set of ad surfaces
// inside one 3D scene (a stadium's hoardings), filled by a SINGLE auction with
// competitive separation: one advertiser per scene. This proves that live — the
// SSP builds one intrinsic scene request, the DSP returns every advertiser's
// product as a slate, and the exchange's Batch strategy assigns the surfaces so
// no advertiser appears twice.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestInGameSceneCompetitiveSeparation(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "ingame")

	// Three DISTINCT advertisers, each with one product eligible on geo/device.
	// Advertiser "cola" runs TWO products — the scene must still give it only one
	// surface (competitive separation), leaving room for the other two brands.
	cola := h.CreateAdvertiser(t, "adv-cola")
	pepsi := h.CreateAdvertiser(t, "adv-pepsi")
	acme := h.CreateAdvertiser(t, "adv-acme-ingame")
	for _, adv := range []harness.Account{cola, pepsi, acme} {
		h.GrantBalance(t, adv.ID, 100_000, "e2e-ingame-grant")
	}
	colaIO := h.CreateInsertionOrder(t, cola, "e2e-ig-io-cola", 5000)
	pepsiIO := h.CreateInsertionOrder(t, pepsi, "e2e-ig-io-pepsi", 5000)
	acmeIO := h.CreateInsertionOrder(t, acme, "e2e-ig-io-acme", 5000)

	tgt := harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}}
	// Cola bids highest on BOTH its products — a pure-price auction would hand it
	// two surfaces. Competitive separation must stop that.
	// Bids sit above BuildBasicWorld's own $3.50 display campaign so our three
	// advertisers are the top three in the slate (the basic campaign is the 4th
	// bid and drops on the 3-surface limit).
	h.CreateCampaign(t, cola, colaIO, "e2e-ig-cola-1", 9.00, 500, "e2e-ig-cr-cola-1", "cola.test", tgt)
	h.CreateCampaign(t, cola, colaIO, "e2e-ig-cola-2", 8.00, 500, "e2e-ig-cr-cola-2", "cola.test", tgt)
	h.CreateCampaign(t, pepsi, pepsiIO, "e2e-ig-pepsi", 6.00, 500, "e2e-ig-cr-pepsi", "pepsi.test", tgt)
	h.CreateCampaign(t, acme, acmeIO, "e2e-ig-acme", 4.00, 500, "e2e-ig-cr-acme", "acme.test", tgt)
	h.RefreshAllCaches(t)

	// One intrinsic scene with 3 surfaces.
	auc := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID,
		Channel:   "ingame",
		Surfaces:  3,
		Geo:       "GBR",
		Device:    "mobile",
	})
	winners := h.ExtractAllWinners(t, auc)
	if len(winners) != 3 {
		t.Fatalf("want 3 surfaces filled, got %d winners: %+v", len(winners), winners)
	}

	// Competitive separation: every winning advertiser (seat) is distinct — cola
	// holds at most one surface despite bidding the top two prices.
	seats := map[string]int{}
	for _, win := range winners {
		seats[win.Seat]++
	}
	for seat, n := range seats {
		if n > 1 {
			t.Errorf("advertiser %s won %d surfaces, want ≤1 (competitive separation)", seat, n)
		}
	}
	if seats[cola.ID] != 1 || seats[pepsi.ID] != 1 || seats[acme.ID] != 1 {
		t.Errorf("want one surface each for cola/pepsi/acme, got %v", seats)
	}
	// First-price per surface: cola takes its $9 product (not $8), so its winning
	// price is 9.00.
	for _, win := range winners {
		if win.Seat == cola.ID && (win.Price < 8.99 || win.Price > 9.01) {
			t.Errorf("cola surface cleared at %.2f, want 9.00 (its top product, first-price)", win.Price)
		}
	}
}
