//go:build e2e

// Publisher channel inventory: a publisher declares a placement's channel (format)
// and surface count, and the SSP serve honours them WITHOUT any ?channel=/?surfaces=
// query params. This proves an in-game placement declaring 3 surfaces serves a
// 3-surface scene auction end to end — the publisher-side control wired to serving.
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestPublisherChannelInventory(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pub-chan")

	// Three advertisers so the in-game scene has distinct demand to separate.
	tgt := harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}}
	for i, spec := range []struct {
		key string
		bid float64
	}{{"cola", 9.00}, {"pepsi", 6.00}, {"acme", 4.00}} {
		adv := h.CreateAdvertiser(t, "adv-pubchan-"+spec.key)
		h.GrantBalance(t, adv.ID, 100_000, "e2e-pubchan-grant")
		io := h.CreateInsertionOrder(t, adv, "e2e-pc-io-"+spec.key, 5000)
		h.CreateCampaign(t, adv, io, "e2e-pc-"+spec.key, spec.bid, 500, "e2e-pc-cr-"+spec.key, spec.key+".test", tgt)
		_ = i
	}

	// A publisher-declared in-game placement: format=ingame, 3 scene surfaces.
	pl := h.AddChannelPlacement(t, w.Publisher, "e2e-pubchan-ingame", 300, 250, 1.00, "ingame", 3)
	h.RefreshAllCaches(t)

	// Serve with NO channel/surfaces params — both come from the placement.
	auc := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: pl.ExternalID, Geo: "GBR", Device: "mobile",
	})
	winners := h.ExtractAllWinners(t, auc)
	if len(winners) != 3 {
		t.Fatalf("want 3 surface winners from the placement's declared channel+surfaces, got %d: %+v", len(winners), winners)
	}
	seats := map[string]int{}
	for _, win := range winners {
		seats[win.Seat]++
	}
	if len(seats) != 3 {
		t.Errorf("want 3 distinct advertisers across the scene surfaces (competitive separation), got %v", seats)
	}
}
