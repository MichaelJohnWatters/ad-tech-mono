//go:build e2e

// Per-campaign frequency caps via the real API. The ad server already kept a
// per-user-per-campaign impression counter but enforced only the platform
// default limit; now an advertiser's own frequency_cap (limit/window on the
// campaign, stored in targeting_rules.frequency_caps) is loaded into the ad
// server's warm cache and applied per serve.
//
// The freq-cap check runs before creative resolution, so a placeholder
// creative id is fine — the serve is gated on the counter, not the creative.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCampaignFreqCapViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("fcapi-%d", time.Now().UnixNano())
	pub := h.Signup(t, "FCap Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"FCap Site","domain":"`+uniq+`.test"}`)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"FCap MPU","format":"display","width":300,"height":250,"floor_price":0.50
	}`, site["id"]))

	// Campaign with an advertiser-set cap of 2 impressions/user/day.
	adv := h.Signup(t, "FCap Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"FCap","base_bid":3.0,"daily_budget":500,
		"frequency_cap":{"limit":2,"window":"day"}
	}`)
	campaignID := created["id"].(string)
	// Load the new cap into the ad server's freq-cap warm cache.
	h.RefreshAllCaches(t)

	serve := func(trace string) int {
		return h.ServeAd(t, models.ServeRequest{
			TraceID:      trace,
			UserID:       uniq + "-user",
			CampaignID:   campaignID,
			CreativeID:   uniq + "-cr", // unknown → default HTML; freq cap still applies
			PlacementID:  pl["id"].(string),
			PublisherID:  site["id"].(string),
			AdvertiserID: created["account_id"].(string),
			SiteDomain:   uniq + ".test",
			Width:        300, Height: 250, Currency: "USD",
		})
	}

	// Cap is 2: first two serves succeed, the third is blocked.
	if s := serve(uniq + "-1"); s != 200 {
		t.Fatalf("serve #1 status=%d, want 200", s)
	}
	if s := serve(uniq + "-2"); s != 200 {
		t.Fatalf("serve #2 status=%d, want 200", s)
	}
	if s := serve(uniq + "-3"); s != 429 {
		t.Fatalf("serve #3 status=%d, want 429 (advertiser cap of 2 exceeded)", s)
	}

	// A different user is unaffected (cap is per user).
	if s := h.ServeAd(t, models.ServeRequest{
		TraceID: uniq + "-other", UserID: uniq + "-user2",
		CampaignID: campaignID, CreativeID: uniq + "-cr",
		PlacementID: pl["id"].(string), PublisherID: site["id"].(string),
		AdvertiserID: created["account_id"].(string),
		SiteDomain:   uniq + ".test", Width: 300, Height: 250, Currency: "USD",
	}); s != 200 {
		t.Errorf("different-user serve status=%d, want 200 (cap is per user)", s)
	}

	// PATCH the cap away → the capped user can serve again (counter still at 2,
	// but no per-campaign cap now means the platform default of 5 applies).
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID, `{"frequency_cap":{"limit":0}}`)
	h.RefreshAllCaches(t)
	if s := serve(uniq + "-4"); s != 200 {
		t.Errorf("serve after clearing cap status=%d, want 200 (default cap 5 > current count 3)", s)
	}
}
