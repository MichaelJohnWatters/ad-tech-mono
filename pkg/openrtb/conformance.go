package openrtb

import "fmt"

// Conformance validation for partner certification (PLAN Phase 11 #112). These
// checks are the machine-readable half of "is this partner's OpenRTB integration
// spec-correct" — reusable by the partner test-bid/validate endpoints and (later)
// by the exchange to reject malformed inbound demand.

// Severity distinguishes a must-fix violation from a recommendation.
type Severity string

const (
	SevError Severity = "error" // spec violation — the bid/request is invalid
	SevWarn  Severity = "warn"  // allowed but discouraged (quality/interop)
)

// Finding is one conformance result.
type Finding struct {
	Severity Severity `json:"severity"`
	Field    string   `json:"field"`
	Message  string   `json:"message"`
}

// HasErrors reports whether any finding is an error (vs. only warnings).
func HasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == SevError {
			return true
		}
	}
	return false
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// ValidateBidResponse checks a DSP's BidResponse for OpenRTB spec-correctness,
// against the request it answers (req may be nil to skip request-relative checks).
// A genuine NO-BID (explicit NoBid, or no seatbid/bids) is VALID and returns no
// errors — declining to bid is conformant. Returns errors (must-fix) + warnings.
func ValidateBidResponse(req *BidRequest, resp *BidResponse) []Finding {
	var fs []Finding
	if resp == nil {
		return []Finding{{SevError, "", "empty or unparseable response body"}}
	}

	// Count bids to detect a no-bid.
	bids := 0
	for _, sb := range resp.SeatBid {
		bids += len(sb.Bid)
	}
	if resp.NoBid || bids == 0 {
		return fs // valid no-bid — nothing more to check
	}

	if resp.ID == "" {
		fs = append(fs, Finding{SevWarn, "id", "response id is empty (should echo the request id)"})
	} else if req != nil && resp.ID != req.ID {
		fs = append(fs, Finding{SevWarn, "id", "response id does not echo the request id"})
	}

	// Index the request's impressions for impid + floor validation.
	floor := map[string]float64{}
	impKnown := map[string]bool{}
	if req != nil {
		for _, imp := range req.Imp {
			impKnown[imp.ID] = true
			floor[imp.ID] = imp.BidFloor
		}
	}

	for si, sb := range resp.SeatBid {
		for bi, b := range sb.Bid {
			p := fmt.Sprintf("seatbid[%d].bid[%d]", si, bi)
			if b.ImpID == "" {
				fs = append(fs, Finding{SevError, p + ".impid", "impid is required — a bid must reference a request impression"})
			} else if req != nil && !impKnown[b.ImpID] {
				fs = append(fs, Finding{SevError, p + ".impid", "impid " + b.ImpID + " does not match any request impression"})
			}
			if b.Price <= 0 {
				fs = append(fs, Finding{SevError, p + ".price", "price must be > 0"})
			} else if req != nil && impKnown[b.ImpID] && b.Price < floor[b.ImpID] {
				fs = append(fs, Finding{SevError, p + ".price", fmt.Sprintf("price %.4f is below the impression bidfloor %.4f", b.Price, floor[b.ImpID])})
			}
			if b.AdM == "" && b.MediaURL == "" {
				fs = append(fs, Finding{SevError, p + ".adm", "no creative — adm (or the media extension) is required to render"})
			}
			if b.ID == "" {
				fs = append(fs, Finding{SevWarn, p + ".id", "bid id is empty (recommended for tracking)"})
			}
			if b.CrID == "" {
				fs = append(fs, Finding{SevWarn, p + ".crid", "crid is recommended (creative-level reporting + review)"})
			}
			if len(b.ADomain) == 0 {
				fs = append(fs, Finding{SevWarn, p + ".adomain", "adomain is recommended (advertiser-domain quality + blocklist filtering)"})
			} else if req != nil {
				for _, ad := range b.ADomain {
					if contains(req.BAdv, ad) {
						fs = append(fs, Finding{SevError, p + ".adomain", "advertiser domain " + ad + " is in the request's blocked list (badv)"})
					}
				}
			}
		}
	}

	if resp.Cur != "" && req != nil && len(req.Cur) > 0 && !contains(req.Cur, resp.Cur) {
		fs = append(fs, Finding{SevWarn, "cur", "response currency " + resp.Cur + " is not in the request's allowed currency list"})
	}
	return fs
}

// ValidateBidRequest checks a supply partner's (SSP's) inbound BidRequest for
// spec-correctness — enough for the exchange to reject a malformed request.
func ValidateBidRequest(req *BidRequest) []Finding {
	var fs []Finding
	if req == nil {
		return []Finding{{SevError, "", "empty or unparseable request body"}}
	}
	if req.ID == "" {
		fs = append(fs, Finding{SevError, "id", "request id is required"})
	}
	if len(req.Imp) == 0 {
		fs = append(fs, Finding{SevError, "imp", "at least one impression is required"})
	}
	for i, imp := range req.Imp {
		p := fmt.Sprintf("imp[%d]", i)
		if imp.ID == "" {
			fs = append(fs, Finding{SevError, p + ".id", "impression id is required"})
		}
		if imp.Banner == nil && imp.Video == nil && imp.Audio == nil && imp.Native == nil {
			fs = append(fs, Finding{SevError, p, "impression must declare at least one media type (banner/video/audio/native)"})
		}
		if imp.BidFloor > 0 && imp.BidFloorCur == "" {
			fs = append(fs, Finding{SevWarn, p + ".bidfloorcur", "bidfloorcur is recommended when a bidfloor is set (defaults to USD)"})
		}
	}
	if req.Site == nil && req.App == nil {
		fs = append(fs, Finding{SevError, "", "request must carry either a site or an app object"})
	}
	// Supply chain is recommended for transparency; validate it if present.
	if req.Source != nil && req.Source.Ext != nil && req.Source.Ext.SChain != nil {
		if err := ValidateSChain(req.Source.Ext.SChain); err != nil {
			fs = append(fs, Finding{SevWarn, "source.ext.schain", "supply chain is malformed: " + err.Error()})
		}
	} else {
		fs = append(fs, Finding{SevWarn, "source.ext.schain", "no supply chain declared (recommended for transparency)"})
	}
	return fs
}

// GoldenBidRequest returns a representative OpenRTB 2.6 bid request used to probe
// a partner's sandbox endpoint during test-bid / certification. One 300x250
// display impression on a site, a $0.50 floor, USD.
func GoldenBidRequest(id string) BidRequest {
	return BidRequest{
		ID: id,
		Imp: []Imp{{
			ID:          "1",
			Banner:      &Banner{W: 300, H: 250, Mimes: []string{"image/png", "image/jpeg"}},
			BidFloor:    0.50,
			BidFloorCur: "USD",
		}},
		Site:   &Site{Domain: "sandbox.adtech.example", Page: "https://sandbox.adtech.example/article"},
		Device: &Device{UA: "Mozilla/5.0 (sandbox certification probe)", IP: "203.0.113.10"},
		TMax:   100,
		Cur:    []string{"USD"},
	}
}
