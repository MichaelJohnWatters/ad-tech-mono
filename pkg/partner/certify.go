package partner

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"

// Certification (PLAN Phase 11 #112 slice 4): a scored acceptance run. The
// partner runs its bidder against a fixed set of golden scenarios and submits the
// responses; each is validated for OpenRTB conformance. Passing every scenario
// certifies the partner (sandbox → certified). A live probe (slice 3 test-bid)
// checks reachability; certification checks response CORRECTNESS across scenarios
// (including floor respect and blocked-advertiser handling) — the parts a single
// probe can't force.

// Scenario is one golden (request, expectation) the partner must answer correctly.
type Scenario struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Request     openrtb.BidRequest `json:"request"`
}

// CertificationScenarios is the fixed acceptance set. Each request is designed to
// exercise a specific conformance rule; a valid no-bid is always acceptable, so a
// bidder passes by responding correctly (bidding conformantly or declining).
func CertificationScenarios() []Scenario {
	std := openrtb.GoldenBidRequest("cert-standard")

	highFloor := openrtb.GoldenBidRequest("cert-respect-floor")
	highFloor.Imp[0].BidFloor = 999.0 // a bid must clear this or (better) no-bid

	blocked := openrtb.GoldenBidRequest("cert-honour-badv")
	blocked.BAdv = []string{"blocked-advertiser.example"} // must not bid this advertiser

	return []Scenario{
		{"standard", "A normal request — return a conformant bid, or a valid no-bid.", std},
		{"respect_floor", "A $999 floor — bid above it, or no-bid.", highFloor},
		{"honour_badv", "blocked-advertiser.example is blocked (badv) — don't bid it.", blocked},
	}
}

// CheckResult is one scenario's outcome.
type CheckResult struct {
	Scenario string            `json:"scenario"`
	Passed   bool              `json:"passed"`
	Findings []openrtb.Finding `json:"findings"`
}

// CertificationResult is the scored run.
type CertificationResult struct {
	Passed bool          `json:"passed"`
	Score  int           `json:"score"`
	Total  int           `json:"total"`
	Checks []CheckResult `json:"checks"`
}

// ScoreCertification validates each scenario's submitted response against that
// scenario's request. A scenario passes when its response is conformant
// (error-free); a missing response fails. The run passes only when every scenario
// passes.
func ScoreCertification(responses map[string]*openrtb.BidResponse) CertificationResult {
	scenarios := CertificationScenarios()
	res := CertificationResult{Total: len(scenarios), Checks: []CheckResult{}}
	for _, sc := range scenarios {
		cr := CheckResult{Scenario: sc.Name, Findings: []openrtb.Finding{}}
		if resp := responses[sc.Name]; resp == nil {
			cr.Findings = []openrtb.Finding{{Severity: openrtb.SevError, Message: "no response submitted for this scenario"}}
		} else {
			req := sc.Request
			cr.Findings = openrtb.ValidateBidResponse(&req, resp)
		}
		cr.Passed = !openrtb.HasErrors(cr.Findings)
		if cr.Passed {
			res.Score++
		}
		res.Checks = append(res.Checks, cr)
	}
	res.Passed = res.Score == res.Total
	return res
}
