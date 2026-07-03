//go:build e2e

// The onboarding journey — the whole customer path through REAL APIs, no
// direct SQL: publisher signs up and onboards a site + placement; advertiser
// signs up, launches a campaign, and funds the account; then a real auction
// runs on the new inventory and the new campaign wins it.
//
// This doubles as the "seed via APIs" action (tilt trigger seed-via-api):
// unlike cmd/seed's direct-SQL UPSERTs it exercises signup, sessions, RBAC,
// tenant scoping, the management CRUD paths, and cache invalidation — so a
// regression anywhere in the onboarding funnel fails this test, not a
// customer.
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestOnboardingJourney(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	// Reset for determinism: with all seeded campaigns gone, the only
	// possible bidder is the campaign this journey creates.
	h.Reset(t)
	h.ResetBillingLedger(t)

	w := h.BuildWorldViaAPI(t, "journey")

	// The advertiser session sees exactly its own campaign, live.
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/campaigns", nil)
	resp, err := w.Advertiser.Do(req)
	if err != nil {
		t.Fatalf("campaign list: %v", err)
	}
	var campaigns []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&campaigns)
	resp.Body.Close()
	if len(campaigns) != 1 || campaigns[0]["ID"] != w.CampaignID || campaigns[0]["Status"] != "live" {
		t.Fatalf("advertiser campaign list = %v, want exactly the journey campaign, live", campaigns)
	}

	// The publisher session sees exactly its own placement.
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/placements", nil)
	resp, err = w.Publisher.Do(req)
	if err != nil {
		t.Fatalf("placement list: %v", err)
	}
	var placements []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&placements)
	resp.Body.Close()
	if len(placements) != 1 || placements[0]["ID"] != w.PlacementID {
		t.Fatalf("publisher placement list = %v, want exactly the journey placement", placements)
	}

	// A real auction on the API-created placement: the SSP accepts the raw
	// UUID, the exchange fans out, and the only live campaign — created two
	// API calls ago — must win.
	auc := h.RunAuction(t, w.PlacementID, "GBR", "mobile", "journey-user-1")
	var br struct {
		SeatBid []struct {
			Bid []struct {
				Price float64 `json:"price"`
			} `json:"bid"`
		} `json:"seatbid"`
	}
	if err := json.Unmarshal(auc.BidResponse, &br); err != nil {
		t.Fatalf("bid response: %v (%s)", err, auc.BidResponse)
	}
	if len(br.SeatBid) == 0 || len(br.SeatBid[0].Bid) == 0 || br.SeatBid[0].Bid[0].Price <= 0 {
		t.Fatalf("auction on the onboarded placement returned no winning bid: %s", auc.BidResponse)
	}
	t.Logf("journey complete: campaign %s won the auction on placement %s at %.2f",
		w.CampaignID, w.PlacementID, br.SeatBid[0].Bid[0].Price)

	// The money side is wired too: the topup is on the balance.
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/billing/topup", nil)
	resp, err = w.Advertiser.Do(req)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	var bal map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&bal)
	resp.Body.Close()
	if bal["balance"].(float64) != 500 {
		t.Errorf("balance = %v, want 500 from the journey topup", bal["balance"])
	}
}
