//go:build e2e

// Household frequency capping (CTV follow-up to Phase 9 step 90): the SSP
// derives the hh: id from the client IP and forwards it on the ServeRequest;
// the ad server then enforces the campaign cap per household IN ADDITION to
// per user. Co-viewing devices — a CTV and a phone on one home IP with
// different user ids — must exhaust ONE shared cap.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestHouseholdFrequencyCap(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "fc-hh")

	const householdIP = "203.0.113.88"
	// Default adserver cap is 5 per user per campaign. Each serve below uses
	// a DIFFERENT user id, so the per-user counters never exceed 1 — any
	// block can only come from the shared household counter.
	const capLimit = 5

	serve := func(i int) harness.SSPServeResult {
		return h.ServeViaSSP(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: fmt.Sprintf("hh-fc-viewer-%d", i), IP: householdIP,
		})
	}

	t.Run("co_viewers_share_the_cap", func(t *testing.T) {
		for i := 1; i <= capLimit; i++ {
			if res := serve(i); res.NoBid {
				t.Fatalf("serve %d/%d for household should fill, got nobid", i, capLimit)
			}
		}
		if res := serve(capLimit + 1); !res.NoBid {
			t.Errorf("serve %d from a NEW user in the same household should be household-capped, got a fill", capLimit+1)
		}
	})

	t.Run("other_household_unaffected", func(t *testing.T) {
		res := h.ServeViaSSP(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: "hh-fc-other-viewer", IP: "203.0.113.89",
		})
		if res.NoBid {
			t.Errorf("different household hit the first household's cap")
		}
	})
}
