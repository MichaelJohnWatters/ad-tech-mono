package openrtb

import "testing"

func goldenReq() *BidRequest {
	r := GoldenBidRequest("req-1")
	r.BAdv = []string{"blocked.example"}
	return &r
}

// a valid winning response to the golden request.
func goodResp() *BidResponse {
	return &BidResponse{
		ID:  "req-1",
		Cur: "USD",
		SeatBid: []SeatBid{{
			Seat: "acme-dsp",
			Bid: []BidObj{{
				ID: "b1", ImpID: "1", Price: 1.25, AdM: "<div>ad</div>",
				CrID: "cr-9", ADomain: []string{"acme.example"},
			}},
		}},
	}
}

func TestValidateBidResponse_Valid(t *testing.T) {
	if fs := ValidateBidResponse(goldenReq(), goodResp()); HasErrors(fs) {
		t.Errorf("good response flagged errors: %+v", fs)
	}
}

func TestValidateBidResponse_NoBidIsValid(t *testing.T) {
	for _, resp := range []*BidResponse{
		{ID: "req-1"},              // empty seatbid
		{ID: "req-1", NoBid: true}, // explicit no-bid
		{ID: "req-1", SeatBid: []SeatBid{{Seat: "s"}}}, // seat with no bids
	} {
		if fs := ValidateBidResponse(goldenReq(), resp); HasErrors(fs) {
			t.Errorf("no-bid flagged errors: %+v", fs)
		}
	}
}

func TestValidateBidResponse_Violations(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*BidResponse)
		want string // a field substring expected among the ERROR findings
	}{
		{"price zero", func(r *BidResponse) { r.SeatBid[0].Bid[0].Price = 0 }, "price"},
		{"below floor", func(r *BidResponse) { r.SeatBid[0].Bid[0].Price = 0.10 }, "price"}, // golden floor 0.50
		{"missing adm", func(r *BidResponse) { r.SeatBid[0].Bid[0].AdM = "" }, "adm"},
		{"bad impid", func(r *BidResponse) { r.SeatBid[0].Bid[0].ImpID = "99" }, "impid"},
		{"blocked adomain", func(r *BidResponse) { r.SeatBid[0].Bid[0].ADomain = []string{"blocked.example"} }, "adomain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := goodResp()
			c.mut(resp)
			fs := ValidateBidResponse(goldenReq(), resp)
			if !HasErrors(fs) {
				t.Fatalf("expected an error finding, got %+v", fs)
			}
			found := false
			for _, f := range fs {
				if f.Severity == SevError && indexOf(f.Field, c.want) >= 0 {
					found = true
				}
			}
			if !found {
				t.Errorf("no error on field %q: %+v", c.want, fs)
			}
		})
	}
}

func TestValidateBidResponse_NoBidWithBidsIsContradiction(t *testing.T) {
	resp := goodResp()
	resp.NoBid = true // declares no-bid yet carries a bid
	fs := ValidateBidResponse(goldenReq(), resp)
	if !HasErrors(fs) {
		t.Errorf("nobid=true with bids present should error: %+v", fs)
	}
}

func TestValidateBidResponse_MultiImpMultiSeat(t *testing.T) {
	req := GoldenBidRequest("r")
	req.Imp = append(req.Imp, Imp{ID: "2", Banner: &Banner{W: 728, H: 90}, BidFloor: 1.00, BidFloorCur: "USD"})
	// Two seats, one bid each on distinct imps — the bid on imp "2" is BELOW its
	// 1.00 floor, so exactly that one must error.
	resp := &BidResponse{ID: "r", Cur: "USD", SeatBid: []SeatBid{
		{Seat: "s1", Bid: []BidObj{{ID: "a", ImpID: "1", Price: 2.0, AdM: "x", CrID: "c", ADomain: []string{"a.example"}}}},
		{Seat: "s2", Bid: []BidObj{{ID: "b", ImpID: "2", Price: 0.50, AdM: "y", CrID: "c", ADomain: []string{"b.example"}}}},
	}}
	fs := ValidateBidResponse(&req, resp)
	if !HasErrors(fs) {
		t.Fatalf("below-floor bid on imp 2 should error: %+v", fs)
	}
	// The good bid on imp "1" must NOT have produced an error.
	for _, f := range fs {
		if f.Severity == SevError && indexOf(f.Field, "bid[0]") >= 0 && indexOf(f.Field, "seatbid[0]") >= 0 {
			t.Errorf("valid bid on imp 1 wrongly flagged: %+v", f)
		}
	}
}

func TestValidateBidResponse_WarnMissingCrid(t *testing.T) {
	resp := goodResp()
	resp.SeatBid[0].Bid[0].CrID = ""
	fs := ValidateBidResponse(goldenReq(), resp)
	if HasErrors(fs) {
		t.Errorf("missing crid should be a WARN, not an error: %+v", fs)
	}
	if len(fs) == 0 {
		t.Error("expected a warning for missing crid")
	}
}

func TestValidateBidRequest(t *testing.T) {
	good := GoldenBidRequest("r")
	good.Site = &Site{Domain: "x.example"}
	if fs := ValidateBidRequest(&good); HasErrors(fs) {
		t.Errorf("golden request flagged errors: %+v", fs)
	}
	// no imp, no site/app → errors.
	bad := &BidRequest{ID: "r"}
	fs := ValidateBidRequest(bad)
	if !HasErrors(fs) {
		t.Errorf("empty request should error: %+v", fs)
	}
}

// indexOf is a tiny substring search for the test's field-matching.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
