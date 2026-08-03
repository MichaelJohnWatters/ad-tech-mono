//go:build e2e

// In-game per-surface billing: a scene auction fills N surfaces with N different
// advertisers, and EACH surface bills independently. The exchange gives each
// winner a distinct per-surface sub-trace (BidObj.ID); firing that surface's
// impression on the sub-trace records + bills it on its own, so a 3-surface scene
// books 3 impressions at the sum of the 3 surface prices — no dedup collapse.
package e2e

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestInGamePerSurfaceBilling(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "ig-bill")

	tgt := harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}}
	for _, spec := range []struct {
		key string
		bid float64
	}{{"cola", 9.00}, {"pepsi", 6.00}, {"acme", 4.00}} {
		adv := h.CreateAdvertiser(t, "adv-igbill-"+spec.key)
		h.GrantBalance(t, adv.ID, 100_000, "e2e-igbill-grant")
		io := h.CreateInsertionOrder(t, adv, "e2e-igb-io-"+spec.key, 5000)
		h.CreateCampaign(t, adv, io, "e2e-igb-"+spec.key, spec.bid, 500, "e2e-igb-cr-"+spec.key, spec.key+".test", tgt)
	}
	h.RefreshAllCaches(t)

	auc := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Channel: "ingame", Surfaces: 3, Geo: "GBR", Device: "mobile",
	})
	winners := h.ExtractAllWinners(t, auc)
	if len(winners) != 3 {
		t.Fatalf("want 3 surface winners, got %d", len(winners))
	}

	// Fire each surface's impression on its own sub-trace (the exchange put it in
	// BidObj.ID). Each is a distinct billable impression.
	var wantMicros int64
	for _, win := range winners {
		if win.ImpressionID == "" {
			t.Fatalf("winner has no per-surface impression id: %+v", win)
		}
		h.FireImpression(t, win.ImpressionID, win.CampaignID, win.CreativeID, auc.PlacementID, auc.PublisherID, win.Seat, "USD", win.Price)
		wantMicros += int64(math.Round(win.Price / 1000 * 1e6)) // per-surface cost = CPM/1000
	}

	// All three surfaces land as distinct impressions (sub-traces of the auction).
	like := fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id LIKE '%s::s%%'", auc.TraceID)
	waitCH(t, h, like, "the 3 surface impressions")
	deadline := time.Now().Add(20 * time.Second)
	for h.ClickHouseScalar(t, like) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d surface impressions recorded, want 3", h.ClickHouseScalar(t, like))
		}
		time.Sleep(time.Second)
	}

	// Money is exact: the booked spend across the surfaces = sum of the per-surface
	// prices (NOT collapsed to one).
	gotMicros := int64(h.ClickHouseScalar(t, fmt.Sprintf(
		"SELECT toInt64(round(sum(clearing_price_usd)*1000000)) FROM adtech.impressions WHERE trace_id LIKE '%s::s%%'", auc.TraceID)))
	if gotMicros != wantMicros {
		t.Errorf("booked scene spend = %d µ$, want %d (sum of the 3 surface prices)", gotMicros, wantMicros)
	}

	// The single-source-of-cost auction_wins table also records all 3 surfaces.
	if n := h.ClickHouseScalar(t, fmt.Sprintf("SELECT count() FROM adtech.auction_wins WHERE trace_id LIKE '%s::s%%'", auc.TraceID)); n != 3 {
		t.Errorf("auction_wins recorded %d surfaces, want 3", n)
	}
}
