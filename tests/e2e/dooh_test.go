//go:build e2e

// Digital-out-of-home end-to-end. DOOH's distinctive mechanic: a screen plays an
// ad ONCE (one proof-of-play beacon) but delivers N impressions to the audience in
// front of it, and the buy is priced per that audience. This proves the money
// spine carries it losslessly — one play → N impressions at N× the per-impression
// cost — and that the DOOH serve routes through the TimeSlot strategy.
package e2e

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDOOHAudienceMultiplier(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dooh")

	// The DOOH serve path: channel=dooh routes through the exchange's TimeSlot
	// strategy (imp.ext.channel=dooh, banner-shaped screen imp) and fills with the
	// banner campaign. A non-nobid result proves the serve wired end to end.
	serve := h.ServeViaSSP(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Channel: "dooh", Geo: "GBR", Device: "mobile",
	})
	if serve.NoBid {
		t.Fatal("DOOH serve did not fill (channel=dooh should route to TimeSlot and win)")
	}

	// Clean auction to get the winner's price for the money math.
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winning bid")
	}

	const mult = 50 // venue audience per play
	perImp := win.Price / 1000
	wantMicros := int64(math.Round(perImp * float64(mult) * 1e6)) // full play cost in µ$

	// One screen play delivering 50 audience impressions.
	h.FireDOOHPlay(t, auc.TraceID, win.CampaignID, win.CreativeID,
		auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, mult)

	// Exactly ONE impression row (one play), tagged dooh, carrying impression_qty=50.
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id='%s'", auc.TraceID),
		"the DOOH play recorded")
	if rows := h.ClickHouseScalar(t, fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id='%s'", auc.TraceID)); rows != 1 {
		t.Fatalf("impression rows = %d, want 1 (one play = one proof-of-play event)", rows)
	}
	if qty := h.ClickHouseScalar(t, fmt.Sprintf("SELECT impression_qty FROM adtech.impressions WHERE trace_id='%s'", auc.TraceID)); qty != mult {
		t.Errorf("impression_qty = %d, want %d (the venue audience per play)", qty, mult)
	}
	if ch := h.ClickHouseScalar(t, fmt.Sprintf("SELECT count() FROM adtech.impressions WHERE trace_id='%s' AND channel='dooh'", auc.TraceID)); ch != 1 {
		t.Errorf("channel=dooh rows = %d, want 1", ch)
	}
	// The true audience count is SUM(impression_qty), not the row count.
	if aud := h.ClickHouseScalar(t, fmt.Sprintf("SELECT toInt64(sum(impression_qty)) FROM adtech.impressions WHERE trace_id='%s'", auc.TraceID)); aud != mult {
		t.Errorf("SUM(impression_qty) = %d, want %d audience impressions", aud, mult)
	}
	// Money is exact: the booked cost is the FULL play cost = per-impression cost × 50.
	gotMicros := int64(h.ClickHouseScalar(t, fmt.Sprintf(
		"SELECT toInt64(round(sum(clearing_price_usd)*1000000)) FROM adtech.impressions WHERE trace_id='%s'", auc.TraceID)))
	if gotMicros != wantMicros {
		t.Errorf("booked play cost = %d µ$, want %d (per-imp %.6f × %d)", gotMicros, wantMicros, perImp, mult)
	}
}
