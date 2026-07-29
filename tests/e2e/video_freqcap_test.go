//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/pages"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestVideoHouseholdFrequencyCap proves the ad server's frequency cap now
// applies to the VIDEO render path. Video ads are built by the publisher-
// adserver, so they used to short-circuit in the SSP without ever calling the
// ad server — silently skipping BOTH the per-user and the per-household cap.
// The household cap exists specifically for co-viewing CTV, so this was the one
// place it most needed to work.
//
// Co-viewing model: distinct users (devices) on ONE end-user IP share a
// household. With the default cap of 5, the household fills 5 video ads for a
// campaign, then the 6th co-viewing device is capped — while a DIFFERENT
// household still fills (proving it's the cap, not budget exhaustion).
func TestVideoHouseholdFrequencyCap(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildPagesWorld(t, h)
	vid := w.PlacementByFormat[pages.Video]

	const limit = 5 // keys.AdServer.FreqCapPerUserPerCampaign default
	household := "203.0.113.7"

	fills := 0
	for i := 0; i < limit+1; i++ {
		// Distinct user per request (a different device in the home), same IP →
		// same household. The user cap never trips (1 each); the household cap does.
		if h.ServeVideoFills(t, vid, fmt.Sprintf("hh-device-%d", i), household) {
			fills++
		}
	}
	if fills != limit {
		t.Errorf("video household cap: %d of %d filled, want exactly %d (cap not enforced on the video path)", fills, limit+1, limit)
	}

	// A different household (different IP) is unaffected — confirms the decline
	// above is the per-household frequency cap, not campaign/budget exhaustion.
	if !h.ServeVideoFills(t, vid, "other-home", "203.0.113.8") {
		t.Error("a fresh household should still fill video — the cap must be per-household, not global")
	}
}
