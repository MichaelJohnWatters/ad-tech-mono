//go:build e2e

// Staleness bound of the in-process audience L1 cache (pkg/audience/store/l1).
//
// The L1 cache serves a user's segments from memory for a short ABSOLUTE TTL
// (default 3s). The subtle staleness vector it introduces is the NEGATIVE cache:
// resolving a user who is NOT yet a member caches "no segments" for the TTL. A
// freshly-enrolled user must still become targetable within the bound
// (TTL + the changelog drainer window), not be stuck behind a stale empty
// entry forever. This test forces that exact case and asserts the bound — a
// regression guard against the cache TTL being cranked too high or a hit
// wrongly extending expiry.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAudienceL1CacheStalenessBounded(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "l1-stale")
	uniq := fmt.Sprintf("l1stale-%d", time.Now().UnixNano())

	// Segment + campaign targeting it, other gates open. Debug refresh is fine
	// in SETUP — it's the post-enroll propagation (below) that must happen with
	// no help, through the L1 cache's TTL.
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

	newUser := uniq + "-joiner"

	// Force the L1 NEGATIVE cache: resolve the non-member first so the SSP/DSP
	// cache "this user has no segments" for the TTL. This is the staleness vector
	// the L1 cache adds on top of the Redis read path.
	if winFor(newUser) {
		t.Fatal("baseline: non-member won before the membership upload")
	}

	// Enroll via the production upload path — no debug refresh from here.
	h.UploadAudience(t, w.AdvAcc.ID, uniq+"-list", "public", []string{newUser})

	// Despite the negative-cache entry, the user must become targetable within
	// the L1 TTL (default 3s) + changelog drainer (~3s) + polling margin.
	deadline := time.Now().Add(12 * time.Second)
	for !winFor(newUser) {
		if time.Now().After(deadline) {
			t.Fatal("freshly-enrolled user not targetable within 12s — L1 cache staleness exceeds the TTL+drainer bound (negative-cache not expiring, or a hit wrongly extended its TTL)")
		}
		time.Sleep(time.Second)
	}
}
