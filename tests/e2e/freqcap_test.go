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

// TestFreqCapWindowExpiry — flip adserver.freq_cap_window to a short
// value via the live-config API, saturate the cap, sleep past the
// window, then assert the next serve succeeds. Proves the TTL on the
// Redis counter is wired to the config knob (not hardcoded) and that
// the counter genuinely resets when it expires.
func TestFreqCapWindowExpiry(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "fc-expiry")

	const pod = "adserver-0"
	const key = "adserver.freq_cap_window"

	before := resolveConfig(t, h, key, pod)
	h.SetConfigForPod(t, key, "2s", pod)
	t.Cleanup(func() {
		h.SetConfigForPod(t, key, before.Value, pod)
	})

	user := "fc-expiry-user"
	req := models.ServeRequest{
		TraceID: "fc-expiry", UserID: user,
		CampaignID: w.Campaign.ID, CreativeID: w.Campaign.CreativeID,
		PlacementID: w.Placement.ID, PublisherID: w.Publisher.ID,
		AdvertiserID: w.AdvAcc.ID,
		SiteDomain:   w.Publisher.Domain, Width: 300, Height: 250, Currency: "USD",
	}

	// Saturate: default cap is 5/window.
	for i := 0; i < 5; i++ {
		if s := h.ServeAd(t, req); s != 200 {
			t.Fatalf("warm-up serve #%d status=%d, want 200", i+1, s)
		}
	}
	if s := h.ServeAd(t, req); s != 429 {
		t.Fatalf("over-cap serve status=%d, want 429 (cap should be saturated)", s)
	}

	// Sleep past the 2s window. The Redis TTL was set at the first serve
	// (Incr + Expire-on-first), so add a small margin for clock skew.
	time.Sleep(3 * time.Second)

	if s := h.ServeAd(t, req); s != 200 {
		t.Fatalf("post-expiry serve status=%d, want 200 (window should have reset the counter)", s)
	}
}
