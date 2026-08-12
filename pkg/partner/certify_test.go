package partner

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// conformant returns a valid response to a scenario request (a no-bid, which is
// always conformant — the simplest way to pass a scenario).
func noBid() *openrtb.BidResponse { return &openrtb.BidResponse{NoBid: true} }

func TestScoreCertification_AllNoBidPasses(t *testing.T) {
	resp := map[string]*openrtb.BidResponse{}
	for _, sc := range CertificationScenarios() {
		resp[sc.Name] = noBid()
	}
	res := ScoreCertification(resp)
	if !res.Passed || res.Score != res.Total {
		t.Errorf("all-no-bid should pass: %+v", res)
	}
}

func TestScoreCertification_MissingResponseFails(t *testing.T) {
	resp := map[string]*openrtb.BidResponse{"standard": noBid()} // others missing
	res := ScoreCertification(resp)
	if res.Passed {
		t.Errorf("missing scenario responses must fail: %+v", res)
	}
	if res.Score != 1 {
		t.Errorf("score = %d, want 1 (only 'standard' answered)", res.Score)
	}
}

func TestScoreCertification_NonConformantFails(t *testing.T) {
	resp := map[string]*openrtb.BidResponse{}
	for _, sc := range CertificationScenarios() {
		resp[sc.Name] = noBid()
	}
	// A bid on the blocked advertiser must fail the honour_badv scenario.
	resp["honour_badv"] = &openrtb.BidResponse{
		ID: "cert-honour-badv", SeatBid: []openrtb.SeatBid{{Bid: []openrtb.BidObj{
			{ID: "b", ImpID: "1", Price: 2.0, AdM: "x", ADomain: []string{"blocked-advertiser.example"}},
		}}},
	}
	res := ScoreCertification(resp)
	if res.Passed {
		t.Errorf("bidding a blocked advertiser must fail certification: %+v", res)
	}
	// Exactly the honour_badv check should be the failing one.
	for _, c := range res.Checks {
		if c.Scenario == "honour_badv" && c.Passed {
			t.Error("honour_badv should have failed")
		}
	}
}
