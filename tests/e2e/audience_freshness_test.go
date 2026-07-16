//go:build e2e

// Cache-freshness feedback loop, tested the way production experiences it:
// NO debug cache refresh after the membership write. The gateway upload
// publishes adtech.cache.invalidate.audience; the SSP/DSP preloaders (now
// actually subscribed — the subject previously had zero subscribers) run a
// debounced refresh, and the new member must win a real auction within
// seconds — far inside the 30s poll interval that used to be the only
// freshness mechanism.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAudienceFreshnessViaInvalidate(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "aud-fresh")
	uniq := fmt.Sprintf("fresh-%d", time.Now().UnixNano())

	// Setup (debug refresh allowed HERE — it's the membership write below
	// that must propagate without help): create the segment, point the
	// campaign at it, open the other targeting gates.
	segID := h.UploadAudience(t, w.AdvAcc.ID, uniq+"-list", "public", []string{uniq + "-seed"})
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})
	h.RefreshAllCaches(t)

	winFor := func(userID string) bool {
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: userID,
		})
		win := h.ExtractWinner(t, res)
		return !win.NoBid && win.CampaignID == w.Campaign.ID
	}

	// Baseline: the new user is not yet a member → no-bid.
	newUser := uniq + "-late-joiner"
	if winFor(newUser) {
		t.Fatal("baseline: non-member won before the membership upload")
	}

	// The write under test: add the member via the production upload path.
	// From here on, NO debug refreshes — freshness must come from the
	// invalidate → debounced preload.
	h.UploadAudience(t, w.AdvAcc.ID, uniq+"-list", "public", []string{newUser})

	deadline := time.Now().Add(10 * time.Second)
	for !winFor(newUser) {
		if time.Now().After(deadline) {
			t.Fatal("new member not targetable within 10s of upload — invalidate → preloader refresh isn't propagating (poll interval alone is 30s)")
		}
		time.Sleep(time.Second)
	}
}
