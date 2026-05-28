package openrtb_test

import (
	"encoding/json"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

func TestBidRequest_JSON_RoundTrip(t *testing.T) {
	req := openrtb.BidRequest{
		ID: "auction-123",
		Imp: []openrtb.Imp{
			{
				ID:       "1",
				Banner:   &openrtb.Banner{W: 300, H: 250},
				BidFloor: 1.50,
			},
		},
		Site: &openrtb.Site{
			Domain: "news.example.com",
			Page:   "https://news.example.com/sports/football",
			Cat:    []string{"IAB17"},
		},
		Device: &openrtb.Device{
			UA:         "Mozilla/5.0",
			IP:         "203.0.113.42",
			DeviceType: 4,
			Geo:        &openrtb.Geo{Country: "GBR", City: "London"},
		},
		User: &openrtb.User{
			ID: "user-abc",
			Ext: &openrtb.UserExt{
				Segments: []string{"sports_fans"},
			},
		},
		TMax: 100,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded openrtb.BidRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.ID != "auction-123" {
		t.Errorf("ID = %q, want auction-123", decoded.ID)
	}
	if len(decoded.Imp) != 1 || decoded.Imp[0].Banner.W != 300 {
		t.Error("imp not decoded correctly")
	}
	if decoded.Site.Domain != "news.example.com" {
		t.Errorf("site domain = %q, want news.example.com", decoded.Site.Domain)
	}
	if decoded.Device.Geo.Country != "GBR" {
		t.Errorf("geo country = %q, want GBR", decoded.Device.Geo.Country)
	}
	if decoded.User.Ext.Segments[0] != "sports_fans" {
		t.Error("user segments not decoded")
	}
}

func TestBidResponse_JSON_RoundTrip(t *testing.T) {
	resp := openrtb.BidResponse{
		ID: "auction-123",
		SeatBid: []openrtb.SeatBid{
			{
				Bid: []openrtb.BidObj{
					{
						ID:      "bid-1",
						ImpID:   "1",
						Price:   2.50,
						CID:     "campaign-456",
						CrID:    "creative-789",
						ADomain: []string{"acme.com"},
					},
				},
				Seat: "dsp-1",
			},
		},
		Cur: "USD",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded openrtb.BidResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.ID != "auction-123" {
		t.Errorf("ID = %q, want auction-123", decoded.ID)
	}
	if len(decoded.SeatBid) != 1 {
		t.Fatal("expected 1 seatbid")
	}
	bid := decoded.SeatBid[0].Bid[0]
	if bid.Price != 2.50 {
		t.Errorf("price = %f, want 2.50", bid.Price)
	}
	if bid.CID != "campaign-456" {
		t.Errorf("CID = %q, want campaign-456", bid.CID)
	}
}

func TestBidResponse_NoBid(t *testing.T) {
	resp := openrtb.BidResponse{
		ID:    "auction-123",
		NoBid: true,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded openrtb.BidResponse
	json.Unmarshal(data, &decoded)

	if !decoded.NoBid {
		t.Error("NoBid should be true")
	}
	if len(decoded.SeatBid) != 0 {
		t.Error("NoBid response should have no seatbids")
	}
}

func TestBidRequest_WithApp(t *testing.T) {
	req := openrtb.BidRequest{
		ID: "app-auction",
		Imp: []openrtb.Imp{
			{ID: "1", Banner: &openrtb.Banner{W: 320, H: 50}},
		},
		App: &openrtb.App{
			Bundle: "com.example.app",
			Name:   "Example App",
			Cat:    []string{"IAB9-30"},
		},
	}

	data, _ := json.Marshal(req)
	var decoded openrtb.BidRequest
	json.Unmarshal(data, &decoded)

	if decoded.App.Bundle != "com.example.app" {
		t.Errorf("app bundle = %q, want com.example.app", decoded.App.Bundle)
	}
	if decoded.Site != nil {
		t.Error("app request should not have site")
	}
}

func TestBidRequest_WithRegs(t *testing.T) {
	req := openrtb.BidRequest{
		ID:  "gdpr-auction",
		Imp: []openrtb.Imp{{ID: "1"}},
		Regs: &openrtb.Regs{
			COPPA: 0,
			Ext: &openrtb.RegsExt{
				GDPR:      1,
				USPrivacy: "1YNN",
			},
		},
	}

	data, _ := json.Marshal(req)
	var decoded openrtb.BidRequest
	json.Unmarshal(data, &decoded)

	if decoded.Regs.Ext.GDPR != 1 {
		t.Errorf("GDPR = %d, want 1", decoded.Regs.Ext.GDPR)
	}
	if decoded.Regs.Ext.USPrivacy != "1YNN" {
		t.Errorf("USPrivacy = %q, want 1YNN", decoded.Regs.Ext.USPrivacy)
	}
}
