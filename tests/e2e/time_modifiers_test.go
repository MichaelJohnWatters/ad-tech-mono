//go:build e2e

// Time-of-day bid modifiers + campaign timezone via the real API. The engine's
// ApplyModifiers already handled TimeOfDay windows, but parseModifiers didn't
// read them, the DSP never set ModifierContext.HourOfDay, and line_items.timezone
// had no runtime consumer. Now the DSP evaluates the hour in the campaign's
// timezone and applies matching time windows.
//
// Determinism: the campaign pins timezone "UTC" and the test computes "now" in
// UTC, so the assertions hold regardless of the DSP pod's local timezone. A
// 4.00 floor with a 3.00 base bid flips win/no-bid on whether a +50% window is
// active (3.00 × 1.5 = 4.50 clears the floor).
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestTimeOfDayModifiersViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("tod-%d", time.Now().UnixNano())
	pub := h.Signup(t, "TOD Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"TOD Site","domain":"`+uniq+`.test"}`)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"TOD MPU","format":"display","width":300,"height":250,"floor_price":4.0
	}`, site["id"]))
	placementID := pl["id"].(string)

	// Campaign bids 3.00 with an all-day (00:00–24:00) +50% time modifier in
	// UTC — always active → 4.50 clears the 4.00 floor.
	adv := h.Signup(t, "TOD Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"TOD","base_bid":3.0,"daily_budget":500,"timezone":"UTC",
		"bid_modifiers":{"time_of_day":[{"start_hour":0,"end_hour":24,"modifier":50}]}
	}`)
	campaignID := created["id"].(string)
	advAccountID := created["account_id"].(string)
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")
	h.RefreshAllCaches(t)

	run := func() harness.AuctionResult {
		return h.RunAuctionWith(t, harness.AuctionParams{
			Placement: placementID, Geo: "USA", Device: "mobile", UserID: uniq + "-u",
		})
	}

	// All-day window active now → win.
	if w := h.ExtractWinner(t, run()); w.NoBid {
		t.Errorf("all-day +50%% modifier auction no-bid, want a win (4.50 clears the 4.00 floor)")
	}

	// PATCH the window to a 1-hour slot starting 3 hours from now (UTC) —
	// guaranteed inactive → 3.00 < 4.00 floor → no-bid.
	s := (time.Now().UTC().Hour() + 3) % 24
	e := (s + 1) % 24
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID, fmt.Sprintf(`{
		"bid_modifiers":{"time_of_day":[{"start_hour":%d,"end_hour":%d,"modifier":50}]}
	}`, s, e))
	h.RefreshAllCaches(t)
	if w := h.ExtractWinner(t, run()); !w.NoBid {
		t.Errorf("inactive-window auction won, want no-bid (modifier window is not active now)")
	}
}
