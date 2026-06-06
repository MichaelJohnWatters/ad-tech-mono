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

// Video bid request round-trip covers the OpenRTB 2.6 fields that the
// rest of Phase 9 (VAST 4.2 generator, ad-pod auction, SSAI stitcher)
// will read off the parsed request. Touches Plcmt/Pos/Skip/BAttr/API/
// PlaybackMethod/companion + the pod fields so a field regression in
// any of them surfaces here rather than at integration time.
func TestBidRequest_Video_RoundTrip(t *testing.T) {
	req := openrtb.BidRequest{
		ID: "video-auction",
		Imp: []openrtb.Imp{{
			ID: "imp-1",
			Video: &openrtb.Video{
				Mimes:          []string{"video/mp4", "video/webm"},
				Protocols:      []int{2, 3, 5, 6, 7}, // VAST 2-4.2
				W:              640,
				H:              360,
				MinDuration:    5,
				MaxDuration:    30,
				Linearity:      1,
				Plcmt:          1, // instream w/ audio
				Pos:            7, // fullscreen
				StartDelay:     0, // pre-roll
				Skip:           1,
				SkipMin:        5,
				SkipAfter:      5,
				BAttr:          []int{13, 17}, // user-initiated mid-roll, flash
				MinBitRate:     400,
				MaxBitRate:     3000,
				PlaybackMethod: []int{2}, // autoplay sound off
				PlaybackEnd:    1,        // complete
				API:            []int{7}, // OMID 1
				CompanionAd: []openrtb.Companion{
					{ID: "c1", W: 300, H: 250, Vcm: 1},
				},
				CompanionType: []int{1, 2},
				// Ad pod: this imp is slot 2 of a 3-spot pre-roll pod.
				PodID:        "pod-pre-1",
				PodSeq:       1,
				SlotInPod:    2,
				RqdDurs:      []int{15, 15, 30},
				MinCPMPerSec: 0.05,
			},
		}},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded openrtb.BidRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v := decoded.Imp[0].Video
	if v == nil {
		t.Fatal("video lost in round-trip")
	}
	if v.Plcmt != 1 || v.Pos != 7 {
		t.Errorf("Plcmt/Pos = %d/%d, want 1/7", v.Plcmt, v.Pos)
	}
	if v.PodID != "pod-pre-1" || v.SlotInPod != 2 {
		t.Errorf("pod fields lost: %+v", v)
	}
	if len(v.RqdDurs) != 3 || v.RqdDurs[2] != 30 {
		t.Errorf("RqdDurs not preserved: %v", v.RqdDurs)
	}
	if v.MinCPMPerSec != 0.05 {
		t.Errorf("MinCPMPerSec = %v, want 0.05", v.MinCPMPerSec)
	}
	if len(v.CompanionAd) != 1 || v.CompanionAd[0].Vcm != 1 {
		t.Errorf("companion ad lost: %v", v.CompanionAd)
	}
}

// Audio bid request round-trip is the DAAST / podcast / streaming-radio
// equivalent of the video test. NVol is the loudness-normalisation hint
// the SSAI server uses to pick the right pre-encoded variant.
func TestBidRequest_Audio_RoundTrip(t *testing.T) {
	req := openrtb.BidRequest{
		ID: "audio-auction",
		Imp: []openrtb.Imp{{
			ID: "imp-1",
			Audio: &openrtb.Audio{
				Mimes:        []string{"audio/mp4", "audio/mpeg"},
				Protocols:    []int{1, 2}, // DAAST 1.0, DAAST 1.0 wrapper
				MinDuration:  10,
				MaxDuration:  30,
				StartDelay:   0,
				MinBitRate:   64,
				MaxBitRate:   192,
				Feed:         2, // podcast
				Stitched:     1, // SSAI
				NVol:         3, // LUFS-normalised
				MaxSeq:       2,
				PodID:        "podcast-mid-1",
				PodSeq:       2,
				SlotInPod:    1,
				MinCPMPerSec: 0.04,
			},
		}},
	}
	data, _ := json.Marshal(req)
	var decoded openrtb.BidRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	a := decoded.Imp[0].Audio
	if a == nil {
		t.Fatal("audio lost")
	}
	if a.Feed != 2 || a.Stitched != 1 || a.NVol != 3 {
		t.Errorf("feed/stitched/nvol = %d/%d/%d, want 2/1/3", a.Feed, a.Stitched, a.NVol)
	}
	if a.PodID != "podcast-mid-1" {
		t.Errorf("PodID = %q", a.PodID)
	}
}

// Video bid response round-trip covers BURL + ad-pod + API/Protocol
// echo-back. AdM holds the VAST XML in real bids; the test uses a
// stub to keep focus on the field plumbing.
func TestBidResponse_Video_RoundTrip(t *testing.T) {
	resp := openrtb.BidResponse{
		ID: "video-auction",
		SeatBid: []openrtb.SeatBid{{
			Seat: "advertiser-1",
			Bid: []openrtb.BidObj{{
				ID:        "bid-1",
				ImpID:     "imp-1",
				Price:     14.50,
				AdM:       `<VAST version="4.2">...</VAST>`,
				ADomain:   []string{"luxauto.com"},
				CrID:      "vast-creative-1",
				W:         640,
				H:         360,
				Dur:       15,
				API:       7, // OMID 1
				Protocol:  7, // VAST 4.2
				BURL:      "https://tracker.example/burl?bid=1",
				PodID:     "pod-pre-1",
				SlotInPod: 2,
			}},
		}},
		Cur: "USD",
	}
	data, _ := json.Marshal(resp)
	var decoded openrtb.BidResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b := decoded.SeatBid[0].Bid[0]
	if b.Protocol != 7 || b.API != 7 {
		t.Errorf("API/Protocol = %d/%d, want 7/7", b.API, b.Protocol)
	}
	if b.BURL == "" || b.PodID != "pod-pre-1" || b.SlotInPod != 2 {
		t.Errorf("response pod/burl fields lost: %+v", b)
	}
	if b.Dur != 15 {
		t.Errorf("Dur = %d, want 15", b.Dur)
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
