//go:build e2e

// Profile Store Phase 4 gate — an audience bid modifier changes a winning
// price. Same deterministic shape as TestBidModifiersViaAPI: a 4.00 floor
// with a 3.00 base bid no-bids, but a +50% audience modifier lifts a segment
// member's bid to 4.50 which clears the floor — so the modifier's effect is
// a clean win/no-bid flip keyed purely on segment membership.
//
// This was DEAD CODE before Phase 4: the DSP built ModifierContext without
// Segments AND parseModifiers dropped the audience key, so segment modifiers
// never priced any bid.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAudienceBidModifierChangesPrice(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("audmod-%d", time.Now().UnixNano())
	pub := h.Signup(t, "AudMod Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"AudMod Site","domain":"`+uniq+`.test"}`)
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"AudMod MPU","format":"display","width":300,"height":250,"floor_price":4.0
	}`, site["id"]))
	placementID := pl["id"].(string)

	adv := h.Signup(t, "AudMod Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"AudMod","base_bid":3.0,"daily_budget":500
	}`)
	campaignID := created["id"].(string)
	advAccountID := created["account_id"].(string)
	h.GrantBalance(t, advAccountID, 10_000, uniq+"-grant")

	// A DSP-private segment (CRM list) with one member — the DSP's own
	// segment union supplies it, nothing rides the bid request.
	member := uniq + "-member"
	segID := h.UploadAudience(t, advAccountID, uniq+"-vip", "dsp_private", []string{member})

	// +50% for the segment: 3.00 → 4.50, clearing the 4.00 floor.
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID,
		fmt.Sprintf(`{"bid_modifiers":{"audience":{%q:50}}}`, segID))
	h.RefreshAllCaches(t)

	run := func(userID string) harness.BidResponseWinner {
		return h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
			Placement: placementID, Geo: "USA", Device: "mobile", UserID: userID,
		}))
	}

	// Segment member: modified 4.50 clears the floor and the win price
	// reflects the modified bid.
	if w := run(member); w.NoBid {
		t.Error("member auction no-bid, want a win (3.00 +50% audience modifier = 4.50 clears the 4.00 floor)")
	} else if w.Price < 4.0 {
		t.Errorf("member win price %.2f, want ≥ 4.00 (modified bid)", w.Price)
	}
	// Non-member: unmodified 3.00 is under the floor.
	if w := run(uniq + "-nobody"); !w.NoBid {
		t.Errorf("non-member auction won at %.2f, want no-bid (3.00 < 4.00 floor)", w.Price)
	}

	// Clearing the modifier flips the member back to no-bid — the segment
	// membership alone (targeting) isn't what won; the PRICE was.
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID, `{"bid_modifiers":{}}`)
	h.RefreshAllCaches(t)
	if w := run(member); !w.NoBid {
		t.Errorf("member auction won after modifier cleared, want no-bid")
	}
}
