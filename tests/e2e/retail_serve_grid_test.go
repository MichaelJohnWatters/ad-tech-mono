//go:build e2e

// Retail sponsored-results grid render: the real /v1/ssp/serve for a retail
// placement returns the N sponsored slots (structured), each with its position,
// price, and per-slot sub-trace impression id — so a retailer's page renders its
// product cards and bills each slot independently.
package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetailServeReturnsSlotGrid(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "retail-grid")

	cat := []string{"IAB18-5"}
	for _, p := range []struct {
		key string
		bid float64
	}{{"shoes", 5.00}, {"shirt", 3.00}, {"socks", 2.00}} {
		h.CreateCampaign(t, w.AdvAcc, w.IO, "e2e-rg-"+p.key, p.bid, 500, "e2e-rg-cr-"+p.key, p.key+".test",
			harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}, Categories: cat})
	}
	h.RefreshAllCaches(t)

	res := h.ServeViaSSP(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Channel: "retail", Categories: "IAB18-5",
		Surfaces: 3, Geo: "GBR", Device: "mobile",
	})
	if res.NoBid {
		t.Fatal("retail serve did not fill")
	}
	if len(res.Slots) != 3 {
		t.Fatalf("serve returned %d sponsored slots, want 3", len(res.Slots))
	}
	// Positions 1..3, distinct per-slot impression sub-traces of this auction.
	seen := map[string]bool{}
	for i, s := range res.Slots {
		if s.Position != i+1 {
			t.Errorf("slot %d has position %d, want %d", i, s.Position, i+1)
		}
		if s.ImpressionID == "" || !strings.Contains(s.ImpressionID, res.TraceID) {
			t.Errorf("slot %d impression id %q is not a sub-trace of %s", i, s.ImpressionID, res.TraceID)
		}
		if seen[s.ImpressionID] {
			t.Errorf("duplicate slot impression id %q", s.ImpressionID)
		}
		seen[s.ImpressionID] = true
	}
}
