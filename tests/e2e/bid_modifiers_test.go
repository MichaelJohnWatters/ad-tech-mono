//go:build e2e

// Bid modifiers via the real API. The CampaignLoader already read the
// bid_modifiers JSONB and the DSP already applied device/geo modifiers before
// the floor check; the management write path (create + PATCH) was the gap.
//
// Deterministic setup: a 4.00 placement floor with a 3.00 base bid no-bids,
// but a +50% device modifier lifts the bid to 4.50 which clears the floor —
// so the modifier's effect is a clean win/no-bid flip, not a fuzzy price check.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestBidModifiersViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("mod-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Mod Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Mod Site","domain":"`+uniq+`.test"}`)
	// Base floor 4.00 — above the 3.00 base bid, below the 4.50 modified bid.
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Mod MPU","format":"display","width":300,"height":250,"floor_price":4.0
	}`, site["id"]))
	placementID := pl["id"].(string)

	// Campaign bids 3.00 with a +50% mobile device modifier.
	adv := h.Signup(t, "Mod Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"Mod","base_bid":3.0,"daily_budget":500,
		"bid_modifiers":{"device":{"mobile":50}}
	}`)
	campaignID := created["id"].(string)
	advAccountID := created["account_id"].(string)
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")
	h.RefreshAllCaches(t)

	run := func(device string) harness.AuctionResult {
		return h.RunAuctionWith(t, harness.AuctionParams{
			Placement: placementID, Geo: "USA", Device: device, UserID: uniq + "-u",
		})
	}

	// mobile: 3.00 × 1.5 = 4.50 ≥ 4.00 floor → wins.
	if w := h.ExtractWinner(t, run("mobile")); w.NoBid {
		t.Errorf("mobile auction no-bid, want a win (3.00 +50%% = 4.50 clears the 4.00 floor)")
	}
	// desktop: no modifier → 3.00 < 4.00 floor → no-bid.
	if w := h.ExtractWinner(t, run("desktop")); !w.NoBid {
		t.Errorf("desktop auction won, want no-bid (unmodified 3.00 is below the 4.00 floor)")
	}

	// PATCH the modifier away → mobile now no-bids too (3.00 < 4.00).
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID, `{"bid_modifiers":{}}`)
	h.RefreshAllCaches(t)
	if w := h.ExtractWinner(t, run("mobile")); !w.NoBid {
		t.Errorf("mobile auction won after modifier cleared, want no-bid")
	}

	// PATCH a geo modifier (+60% USA) → mobile in USA is 3.00 × 1.6 = 4.80 → wins again.
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID, `{"bid_modifiers":{"geo_country":{"USA":60}}}`)
	h.RefreshAllCaches(t)
	if w := h.ExtractWinner(t, run("mobile")); w.NoBid {
		t.Errorf("USA auction no-bid after +60%% geo modifier, want a win (4.80 clears 4.00)")
	}
}
