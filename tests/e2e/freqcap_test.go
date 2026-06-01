//go:build e2e

// Freq cap edge cases beyond the basic block-after-N already covered in
// the compounding narrative (step 14).
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestFreqCapEmptyUserBypass — when UserID is empty (no-consent path),
// the freq cap MUST be skipped, otherwise we'd block legitimate serves
// for users who haven't been ID'd. The implementation special-cases
// empty user_id in cmd/adserver/freqcap.go.
func TestFreqCapEmptyUserBypass(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "fc-anon")

	base := models.ServeRequest{
		CampaignID:    w.Campaign.ID,
		CreativeID:    w.Campaign.CreativeID,
		PlacementID:   w.Placement.ID,
		PublisherID:   w.Publisher.ID,
		AdvertiserID:  w.AdvAcc.ID,
		ClearingPrice: 1.00,
		Currency:      "USD",
		SiteDomain:    w.Publisher.Domain,
		Width:         300,
		Height:        250,
		UserID:        "", // explicit empty
	}
	// 20 serves with empty user_id — all must succeed; freq cap default
	// limit is 5, so a non-bypassed implementation would 429 from #6 on.
	// trace_id can be constant: the ad server doesn't dedup by trace (that's
	// the tracker's job on pixel fires), so reusing the same ID won't mask
	// a cap miss.
	base.TraceID = "fc-anon"
	for i := 1; i <= 20; i++ {
		if status := h.ServeAd(t, base); status != 200 {
			t.Fatalf("serve #%d with empty user_id status=%d, want 200 (cap should bypass)", i, status)
		}
	}
}

// TestFreqCapPerCampaignIsolation — caps are keyed by (user_id, campaign_id);
// hitting the cap on campaign A must not block campaign B for the same user.
func TestFreqCapPerCampaignIsolation(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "fc-iso")

	// Create a second campaign + creative under the same advertiser so
	// the freq cap key differs.
	io2 := h.CreateInsertionOrder(t, w.AdvAcc, "fc-iso-io-2", 1000)
	c2 := h.CreateCampaign(t, w.AdvAcc, io2,
		"fc-iso-li-2",
		2.00, 100,
		"fc-iso-cr-2", "fc-iso-2.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}},
	)
	h.RefreshAllCaches(t)

	user := "fc-iso-user-shared"

	// Saturate campaign 1 (5 serves at default cap).
	for i := 0; i < 5; i++ {
		h.ServeAd(t, models.ServeRequest{
			TraceID: "fc-iso-1", UserID: user,
			CampaignID: w.Campaign.ID, CreativeID: w.Campaign.CreativeID,
			PlacementID: w.Placement.ID, PublisherID: w.Publisher.ID, AdvertiserID: w.AdvAcc.ID,
			SiteDomain: w.Publisher.Domain, Width: 300, Height: 250, Currency: "USD",
		})
	}
	// 6th on campaign 1 must 429.
	if s := h.ServeAd(t, models.ServeRequest{
		TraceID: "fc-iso-1", UserID: user,
		CampaignID: w.Campaign.ID, CreativeID: w.Campaign.CreativeID,
		PlacementID: w.Placement.ID, PublisherID: w.Publisher.ID, AdvertiserID: w.AdvAcc.ID,
		SiteDomain: w.Publisher.Domain, Width: 300, Height: 250, Currency: "USD",
	}); s != 429 {
		t.Fatalf("campaign 1 over-cap status = %d, want 429", s)
	}

	// 1st on campaign 2 with the same user must succeed — different counter.
	if s := h.ServeAd(t, models.ServeRequest{
		TraceID: "fc-iso-2", UserID: user,
		CampaignID: c2.ID, CreativeID: c2.CreativeID,
		PlacementID: w.Placement.ID, PublisherID: w.Publisher.ID, AdvertiserID: w.AdvAcc.ID,
		SiteDomain: w.Publisher.Domain, Width: 300, Height: 250, Currency: "USD",
	}); s != 200 {
		t.Errorf("campaign 2 first serve status = %d, want 200 (cap is per campaign, not per user)", s)
	}
}

// TestFreqCapWindowExpiry — TTL on the freq cap counter expires; after
// the window, the same (user, campaign) pair allows new serves again.
// Default window is 24h which is too long for a real test; the test
// flushes Redis (already part of harness.Reset implicitly via BuildBasicWorld)
// to simulate the expiry, since we don't yet have a per-test override
// of adserver.freq_cap_window.
//
// The "real" assertion would change the config key to e.g. 1s and sleep —
// pending a harness helper to write live config.
func TestFreqCapWindowExpiry(t *testing.T) {
	t.Skip("needs harness.SetConfig to flip adserver.freq_cap_window low for the test; pending config-write helper")
}
