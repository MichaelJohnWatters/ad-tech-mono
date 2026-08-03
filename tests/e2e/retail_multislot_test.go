//go:build e2e

// Retail multi-slot: a results page has N sponsored slots, and the exchange
// returns the top-N relevance-ranked products — each on its own sub-trace so
// every slot bills independently (same mechanism as in-game surfaces). Proves a
// 3-slot page returns 3 ranked winners and firing each slot's impression books
// 3 distinct impressions at the sum of the slot prices.
package e2e

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetailMultiSlotBilling(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "retail-slots")

	// Three in-category sponsored products under the advertiser (retail applies no
	// competitive separation), plus BuildBasicWorld's off-category campaign which
	// ranks below them.
	cat := []string{"IAB18-5"}
	for _, p := range []struct {
		key string
		bid float64
	}{{"shoes", 5.00}, {"shirt", 3.00}, {"socks", 2.00}} {
		h.CreateCampaign(t, w.AdvAcc, w.IO, "e2e-rs-"+p.key, p.bid, 500, "e2e-rs-cr-"+p.key, p.key+".test",
			harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}, Categories: cat})
	}
	h.RefreshAllCaches(t)

	auc := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Channel: "retail", Categories: "IAB18-5",
		Surfaces: 3, Geo: "GBR", Device: "mobile",
	})
	winners := h.ExtractAllWinners(t, auc)
	if len(winners) != 3 {
		t.Fatalf("want 3 sponsored slots, got %d: %+v", len(winners), winners)
	}

	// Fire each slot's impression on its own sub-trace.
	var wantMicros int64
	for _, win := range winners {
		if win.ImpressionID == "" {
			t.Fatalf("slot winner has no per-slot impression id: %+v", win)
		}
		h.FireImpression(t, win.ImpressionID, win.CampaignID, win.CreativeID, auc.PlacementID, auc.PublisherID, win.Seat, "USD", win.Price)
		wantMicros += int64(math.Round(win.Price / 1000 * 1e6))
	}

	like := fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id LIKE '%s::s%%'", auc.TraceID)
	waitCH(t, h, like, "the 3 slot impressions")
	deadline := time.Now().Add(20 * time.Second)
	for h.ClickHouseScalar(t, like) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d slot impressions recorded, want 3", h.ClickHouseScalar(t, like))
		}
		time.Sleep(time.Second)
	}
	gotMicros := int64(h.ClickHouseScalar(t, fmt.Sprintf(
		"SELECT toInt64(round(sum(clearing_price_usd)*1000000)) FROM adtech.impressions WHERE trace_id LIKE '%s::s%%'", auc.TraceID)))
	if gotMicros != wantMicros {
		t.Errorf("booked page spend = %d µ$, want %d (sum of the 3 slot prices)", gotMicros, wantMicros)
	}
}
