package native

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStandardRequestDefaults(t *testing.T) {
	req := StandardRequest(Spec{})
	if req.Ver != Ver {
		t.Errorf("ver = %q, want %q", req.Ver, Ver)
	}
	if req.Context != 1 || req.PlcmtType != 1 {
		t.Errorf("context/plcmt = %d/%d, want 1/1", req.Context, req.PlcmtType)
	}
	// Defaults: title, main image, sponsored — all required.
	if len(req.Assets) != 3 {
		t.Fatalf("assets = %d, want 3 (title, image, sponsored)", len(req.Assets))
	}
	for _, a := range req.Assets {
		if a.Required != 1 {
			t.Errorf("asset %d should be required by default", a.ID)
		}
	}
	if req.Assets[0].Title == nil || req.Assets[0].Title.Len != 90 {
		t.Errorf("title asset missing/wrong len: %+v", req.Assets[0])
	}
	if req.Assets[1].Img == nil || req.Assets[1].Img.Type != ImageTypeMain {
		t.Errorf("main image asset missing/wrong: %+v", req.Assets[1])
	}
}

func TestStandardRequestOptionalAssets(t *testing.T) {
	req := StandardRequest(Spec{WantIcon: true, WantBody: true, WantCTA: true, TitleLen: 50})
	if req.Assets[0].Title.Len != 50 {
		t.Errorf("title len = %d, want 50", req.Assets[0].Title.Len)
	}
	ids := map[int]bool{}
	for _, a := range req.Assets {
		ids[a.ID] = true
	}
	for _, want := range []int{AssetIDTitle, AssetIDMainImage, AssetIDSponsored, AssetIDIcon, AssetIDBody, AssetIDCTA} {
		if !ids[want] {
			t.Errorf("expected asset id %d present", want)
		}
	}
}

func TestMarshalRequestRoundTrip(t *testing.T) {
	req := StandardRequest(Spec{})
	s, err := MarshalRequest(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(s, `"ver":"1.2"`) {
		t.Errorf("request JSON missing ver: %s", s)
	}
	var back Request
	if err := json.Unmarshal([]byte(s), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Assets) != len(req.Assets) {
		t.Errorf("round-trip asset count = %d, want %d", len(back.Assets), len(req.Assets))
	}
}

func TestBuildResponseOnlyIncludesPresentAssets(t *testing.T) {
	resp := BuildResponse(AssetSet{
		Title:      "Big Sale",
		MainImage:  "https://cdn/img.jpg",
		MainImageW: 1200, MainImageH: 627,
		Sponsored:  "Acme",
		LandingURL: "https://acme.example/deal",
	}, []string{"https://track/imp"}, []string{"https://track/click"})

	if got := len(resp.Native.Assets); got != 3 {
		t.Fatalf("assets = %d, want 3 (title, image, sponsored — body/cta/icon empty)", got)
	}
	if resp.Native.Link.URL != "https://acme.example/deal" {
		t.Errorf("link = %q", resp.Native.Link.URL)
	}
	if len(resp.Native.EventTrackers) != 1 || resp.Native.EventTrackers[0].Event != EventTypeImpression {
		t.Errorf("expected one impression event tracker, got %+v", resp.Native.EventTrackers)
	}
	// Legacy imptrackers mirrored for 1.1 players.
	if len(resp.Native.ImpTrackers) != 1 {
		t.Errorf("expected imptrackers mirror, got %v", resp.Native.ImpTrackers)
	}
	if len(resp.Native.Link.ClickTrackers) != 1 {
		t.Errorf("expected click tracker on link, got %v", resp.Native.Link.ClickTrackers)
	}
}

func TestMarshalResponseShape(t *testing.T) {
	resp := BuildResponse(AssetSet{Title: "T", LandingURL: "https://x"}, nil, nil)
	adm, err := MarshalResponse(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The OpenRTB native response is wrapped in a top-level "native" object.
	if !strings.HasPrefix(adm, `{"native":`) {
		t.Errorf("response must be wrapped in {\"native\":...}, got %s", adm)
	}
	parsed, err := ParseResponse(adm)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Native.Assets[0].Title.Text != "T" {
		t.Errorf("round-trip title = %q, want T", parsed.Native.Assets[0].Title.Text)
	}
}

func TestValidate(t *testing.T) {
	req := StandardRequest(Spec{}) // title, image, sponsored all required
	good := BuildResponse(AssetSet{
		Title: "T", MainImage: "https://i", Sponsored: "Acme", LandingURL: "https://l",
	}, nil, nil)
	if err := Validate(req, good); err != nil {
		t.Errorf("expected valid, got %v", err)
	}

	tests := []struct {
		name string
		resp Response
	}{
		{"missing link", BuildResponse(AssetSet{Title: "T", MainImage: "i", Sponsored: "A"}, nil, nil)},
		{"missing required image", BuildResponse(AssetSet{Title: "T", Sponsored: "A", LandingURL: "https://l"}, nil, nil)},
		{"missing required title", BuildResponse(AssetSet{MainImage: "i", Sponsored: "A", LandingURL: "https://l"}, nil, nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(req, tt.resp); err == nil {
				t.Errorf("expected validation error for %s", tt.name)
			}
		})
	}
}

func TestValidateWrongAssetKind(t *testing.T) {
	req := StandardRequest(Spec{})
	// Fill the title slot (id 1) with a data asset instead of a title.
	resp := Response{Native: ResponseBody{
		Link: Link{URL: "https://l"},
		Assets: []RespAsset{
			{ID: AssetIDTitle, Data: &DataResp{Value: "oops"}},
			{ID: AssetIDMainImage, Img: &ImgResp{URL: "i"}},
			{ID: AssetIDSponsored, Data: &DataResp{Value: "A"}},
		},
	}}
	if err := Validate(req, resp); err == nil {
		t.Error("expected error when required title slot filled by a data asset")
	}
}
