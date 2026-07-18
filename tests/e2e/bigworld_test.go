//go:build e2e

// TestBuildBigWorld is a WORLD BUILDER, not an assertion suite: it stands up
// the "healthy exchange" demand pool for load runs — 10 funded advertisers ×
// 5 campaigns each (50 line items) spread across geos and devices, on top of
// whatever publishers/placements the seed provides. It does NOT reset the
// stack (the point is to leave the world in place) and never runs as part of
// the normal suite: it requires BIGWORLD=1.
//
//	BIGWORLD=1 go test -tags e2e ./tests/e2e/ -run TestBuildBigWorld -count=1
//
// Rationale (2026-07-18 hour run): 13 seeded campaigns left many
// channel/geo combos with zero eligible demand → 46% fill. A ~50-campaign
// pool means the auction usually finds SOMEONE eligible.
package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestBuildBigWorld(t *testing.T) {
	if os.Getenv("BIGWORLD") != "1" {
		t.Skip("world builder — set BIGWORLD=1 to run explicitly")
	}
	h := harness.WaitReady(t, 60*time.Second)

	geos := [][]string{
		{"USA", "CAN"}, {"GBR", "IRL"}, {"DEU", "FRA"}, {"USA", "GBR"},
		{"USA"}, {"GBR"}, {"DEU"}, {"FRA", "ESP"}, {"USA", "DEU", "GBR"}, nil, // nil = no geo filter
	}
	devices := [][]string{
		{"desktop", "mobile"}, {"mobile"}, {"desktop"}, {"mobile", "tablet"}, nil,
	}

	total := 0
	for a := 0; a < 10; a++ {
		key := fmt.Sprintf("bigworld-adv%02d", a)
		adv := h.CreateAdvertiser(t, key)
		h.GrantBalance(t, adv.ID, 50_000, key+"-grant")
		io := h.CreateInsertionOrder(t, adv, key+"-io", 100_000)
		for c := 0; c < 5; c++ {
			ck := fmt.Sprintf("%s-c%d", key, c)
			// Bids 1.5–6.0: overlapping strata so auctions have real
			// competition rather than one dominant campaign.
			bid := 1.5 + float64((a*5+c)%10)*0.45
			h.CreateCampaign(t, adv, io, ck, bid, 5_000, ck+"-cr", "example.com",
				harness.Targeting{Geos: geos[(a+c)%len(geos)], Devices: devices[(a*3+c)%len(devices)]})
			total++
		}
	}
	h.RefreshAllCaches(t)
	t.Logf("big world ready: %d campaigns across 10 funded advertisers", total)
}
