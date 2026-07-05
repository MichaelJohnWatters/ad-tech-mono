//go:build e2e

// Audience segment targeting via the real API. The loader already read
// include/exclude_segments and the DSP already populated the targeting
// Request from User.ext.segments; this proves the remaining plumbing — the
// management API writes the columns and the SSP stamps ?segments= into
// User.ext.segments so the DSP can match on them.
//
// Segments are behavioural, so they only apply when the consent verdict
// allows personalisation; the default (no GDPR/opt-out signals) does.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSegmentTargetingViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("seg-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Seg Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Seg Site","domain":"`+uniq+`.test"}`)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Seg MPU","format":"display","width":300,"height":250,"floor_price":0.50
	}`, site["id"]))
	placementID := pl["id"].(string)

	// One campaign targeting the "auto_intenders" segment.
	adv := h.Signup(t, "Seg Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"Seg","base_bid":3.0,"daily_budget":500,
		"include_segments":["auto_intenders"]
	}`)
	campaignID := created["id"].(string)
	advAccountID := created["account_id"].(string)
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")
	h.RefreshAllCaches(t)

	run := func(segments string) harness.AuctionResult {
		return h.RunAuctionWith(t, harness.AuctionParams{
			Placement: placementID, Geo: "USA", Device: "mobile",
			UserID: uniq + "-u", Segments: segments,
		})
	}

	// Matching segment → win.
	if w := h.ExtractWinner(t, run("auto_intenders")); w.NoBid {
		t.Errorf("auto_intenders auction no-bid, want a win (segment matches)")
	}
	// Different segment → no-bid (segment include gates).
	if w := h.ExtractWinner(t, run("sports_fans")); !w.NoBid {
		t.Errorf("sports_fans auction won, want no-bid (segment include is auto_intenders only)")
	}
	// No segment on the request → no-bid.
	if w := h.ExtractWinner(t, run("")); !w.NoBid {
		t.Errorf("no-segment auction won, want no-bid (segment include requires a match)")
	}

	// PATCH to an exclude rule (clear include): a user in the excluded segment
	// is dropped, everyone else bids.
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID, `{
		"include_segments":[],"exclude_segments":["existing_customers"]
	}`)
	h.RefreshAllCaches(t)
	if w := h.ExtractWinner(t, run("existing_customers")); !w.NoBid {
		t.Errorf("existing_customers auction won, want no-bid (segment excluded)")
	}
	if w := h.ExtractWinner(t, run("new_visitor")); w.NoBid {
		t.Errorf("new_visitor auction no-bid, want a win (not excluded)")
	}
}
