//go:build e2e

// OS / keyword / inventory-type campaign targeting via the real API. The DSP
// targeting engine already evaluated these dimensions; this proves the full
// chain is wired: management API writes the columns, the campaign warm cache
// reads them, the SSP stamps os/keywords into the OpenRTB request, and the DSP
// bid path populates the targeting Request from it.
//
// A single compound-targeted campaign is used so no other campaign can win the
// auction and mask a no-bid (every campaign bids on every auction).
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestOSKeywordTargetingViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("targ-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Targ Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Targ Site","domain":"`+uniq+`.test"}`)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Targ MPU","format":"display","width":300,"height":250,"floor_price":0.50
	}`, site["id"]))
	placementID := pl["id"].(string)

	// One campaign: OS iOS AND keyword "finance" AND inventory "site".
	adv := h.Signup(t, "Targ Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"Targ","base_bid":3.0,"daily_budget":500,
		"include_os":["iOS"],"include_keywords":["finance"],"include_inventory_type":["site"]
	}`)
	campaignID := created["id"].(string)
	advAccountID := created["account_id"].(string)
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")
	h.RefreshAllCaches(t)

	run := func(os, keywords string) harness.AuctionResult {
		return h.RunAuctionWith(t, harness.AuctionParams{
			Placement: placementID, Geo: "USA", Device: "mobile",
			UserID: uniq + "-u", OS: os, Keywords: keywords,
		})
	}

	// All dimensions match → win.
	if w := h.ExtractWinner(t, run("iOS", "finance")); w.NoBid {
		t.Errorf("iOS + finance auction no-bid, want a win (all targeting matches)")
	}
	// Wrong OS → no-bid (OS include gates).
	if w := h.ExtractWinner(t, run("Android", "finance")); !w.NoBid {
		t.Errorf("Android auction won, want no-bid (OS include is iOS only)")
	}
	// Wrong keyword → no-bid (keyword include gates).
	if w := h.ExtractWinner(t, run("iOS", "sports")); !w.NoBid {
		t.Errorf("sports-keyword auction won, want no-bid (keyword include is finance only)")
	}

	// PATCH to app-only inventory + clear OS/keywords: the SSP sends a site
	// request, so nothing should win now (inventory include gates).
	patch := h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID, `{
		"include_os":[],"include_keywords":[],"include_inventory_type":["app"]
	}`)
	_ = patch
	h.RefreshAllCaches(t)
	if w := h.ExtractWinner(t, run("iOS", "finance")); !w.NoBid {
		t.Errorf("app-only inventory auction won on a site request, want no-bid")
	}

	// PATCH to exclude keyword "gambling" (site inventory again): a matching
	// page keyword is excluded, a clean one wins.
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID, `{
		"include_inventory_type":["site"],"exclude_keywords":["gambling"]
	}`)
	h.RefreshAllCaches(t)
	if w := h.ExtractWinner(t, run("iOS", "gambling")); !w.NoBid {
		t.Errorf("gambling-keyword auction won, want no-bid (keyword excluded)")
	}
	if w := h.ExtractWinner(t, run("iOS", "news")); w.NoBid {
		t.Errorf("news-keyword auction no-bid, want a win (not excluded)")
	}
}
