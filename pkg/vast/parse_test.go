package vast

import (
	"strings"
	"testing"
	"time"
)

// extbidderVAST mirrors cmd/extbidder's generated document byte-for-byte in
// shape (VAST 4.0, no xsi attrs, no UniversalAdId) — the exact external
// bid.adm the publisher-adserver must parse and inject into.
const extbidderVAST = `<VAST version="4.0"><Ad id="ext-auc-1"><InLine>` +
	`<AdSystem>extbidder</AdSystem><AdTitle>BrandX (external DSP)</AdTitle>` +
	`<Impression><![CDATA[https://brandx.example/imp?p=${AUCTION_PRICE}]]></Impression>` +
	`<Creatives><Creative><Linear><Duration>00:00:15</Duration>` +
	`<MediaFiles><MediaFile delivery="progressive" type="video/mp4" width="640" height="360"><![CDATA[https://cdn.brandx.example/v.mp4]]></MediaFile></MediaFiles>` +
	`</Linear></Creative></Creatives></InLine></Ad></VAST>`

func TestSniff(t *testing.T) {
	cases := map[string]bool{
		extbidderVAST: true,
		`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<VAST version="4.2"></VAST>`: true,
		`  <VAST version="2.0"></VAST>`:    true,
		`<DAAST version="1.0"></DAAST>`:    false,
		`<html><body>banner</body></html>`: false,
		`{"native":{"ver":"1.2"}}`:         false,
		`https://cdn.example/video.mp4`:    false,
		``:                                 false,
	}
	for adm, want := range cases {
		if got := Sniff(adm); got != want {
			t.Errorf("Sniff(%.40q) = %v, want %v", adm, got, want)
		}
	}
}

func TestParse_ExtbidderVAST40(t *testing.T) {
	doc, err := Parse([]byte(extbidderVAST))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Ads) != 1 || doc.Ads[0].InLine == nil {
		t.Fatalf("expected 1 InLine ad, got %+v", doc.Ads)
	}
	in := doc.Ads[0].InLine
	if in.AdSystem.Name != "extbidder" {
		t.Errorf("AdSystem = %q", in.AdSystem.Name)
	}
	if len(in.Impressions) != 1 || !strings.Contains(in.Impressions[0].URI, "brandx.example/imp") {
		t.Errorf("buyer impression not preserved: %+v", in.Impressions)
	}
	lin := in.Creatives.Creatives[0].Linear
	if lin == nil || time.Duration(lin.Duration) != 15*time.Second {
		t.Fatalf("linear duration not parsed: %+v", lin)
	}
	if len(lin.MediaFiles.MediaFiles) != 1 || !strings.Contains(lin.MediaFiles.MediaFiles[0].URI, "v.mp4") {
		t.Errorf("media file not parsed: %+v", lin.MediaFiles)
	}
}

// TestParse_OwnOutputRoundTrips: our own 4.2 builder output parses back.
func TestParse_OwnOutputRoundTrips(t *testing.T) {
	xmlBytes, err := BuildLinearAd(LinearSpec{
		AdID: "ad-1", AdTitle: "t", Advertiser: "a.test",
		Duration:   15 * time.Second,
		MediaFiles: []MediaFile{{Delivery: "progressive", Type: "video/mp4", URI: "http://cdn/x.mp4"}},
		Trackers:   LinearTrackers{Impression: []string{"http://t/imp"}, Start: []string{"http://t/v?event=start"}},
		ErrorURLs:  []string{"http://t/v?event=error&ec=[ERRORCODE]"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	doc, err := Parse(xmlBytes)
	if err != nil {
		t.Fatalf("parse own output: %v", err)
	}
	if doc.Version != "4.2" || len(doc.Ads) != 1 {
		t.Fatalf("round-trip lost shape: version=%q ads=%d", doc.Version, len(doc.Ads))
	}
}

func TestParse_Rejections(t *testing.T) {
	if _, err := Parse([]byte(`<VAST version="1.0"></VAST>`)); err == nil {
		t.Error("VAST 1.0 must be rejected (ErrVersion)")
	}
	if _, err := Parse([]byte(`<VAST version="5.0"></VAST>`)); err == nil {
		t.Error("VAST 5.0 must be rejected (ErrVersion)")
	}
	if _, err := Parse([]byte(`<DAAST version="1.0"></DAAST>`)); err == nil {
		t.Error("DAAST must be rejected (ErrNotVAST)")
	}
	if _, err := Parse([]byte(`not xml at all`)); err == nil {
		t.Error("garbage must be rejected")
	}
}

// TestInjectLinearTrackers: platform trackers APPEND — the buyer's
// impression, tracking and click-through are preserved; ours ride alongside;
// a buyer ClickThrough is never clobbered (our click URL demotes to a
// tracking pixel); re-injection is a no-op (idempotent per URI).
func TestInjectLinearTrackers(t *testing.T) {
	doc, err := Parse([]byte(extbidderVAST))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	platform := LinearTrackers{
		Impression: []string{"http://t/v1/t/imp?tid=x&sig=s"},
		Start:      []string{"http://t/v1/t/video?event=start&sig=s"},
		Complete:   []string{"http://t/v1/t/video?event=complete&sig=s"},
	}
	errs := []string{"http://t/v1/t/video?event=error&sig=s&ec=[ERRORCODE]"}
	click := ClickSpec{ClickThrough: "http://t/v1/t/click?tid=x&sig=s", ClickTracking: []string{"http://t/v1/t/click?ev=ct&sig=s"}}

	doc.InjectLinearTrackers(platform, errs, click)
	doc.InjectLinearTrackers(platform, errs, click) // idempotence: second pass adds nothing

	in := doc.Ads[0].InLine
	if len(in.Impressions) != 2 {
		t.Fatalf("want buyer + platform impressions (2), got %d: %+v", len(in.Impressions), in.Impressions)
	}
	if !strings.Contains(in.Impressions[0].URI, "brandx.example") {
		t.Errorf("buyer impression must stay first: %+v", in.Impressions)
	}
	if len(in.Errors) != 1 || !strings.Contains(in.Errors[0].URI, "ec=[ERRORCODE]") {
		t.Errorf("platform error URI not injected: %+v", in.Errors)
	}
	lin := in.Creatives.Creatives[0].Linear
	if lin.TrackingEvents == nil || len(lin.TrackingEvents.Tracking) != 2 {
		t.Fatalf("want 2 injected trackings, got %+v", lin.TrackingEvents)
	}
	// Buyer had no ClickThrough → ours is set.
	if lin.VideoClicks == nil || lin.VideoClicks.ClickThrough == nil || !strings.Contains(lin.VideoClicks.ClickThrough.URI, "/v1/t/click") {
		t.Fatalf("platform ClickThrough not set on empty buyer slot: %+v", lin.VideoClicks)
	}
	if len(lin.VideoClicks.ClickTracking) != 1 {
		t.Errorf("want 1 click tracker, got %+v", lin.VideoClicks.ClickTracking)
	}

	// Buyer WITH a ClickThrough: ours must not clobber — it demotes to tracking.
	doc2, _ := Parse([]byte(extbidderVAST))
	lin2 := doc2.Ads[0].InLine.Creatives.Creatives[0].Linear
	lin2.VideoClicks = &VideoClicks{ClickThrough: &ClickURL{URI: "https://brandx.example/landing"}}
	doc2.InjectLinearTrackers(LinearTrackers{}, nil, click)
	vc := doc2.Ads[0].InLine.Creatives.Creatives[0].Linear.VideoClicks
	if vc.ClickThrough.URI != "https://brandx.example/landing" {
		t.Errorf("buyer ClickThrough clobbered: %q", vc.ClickThrough.URI)
	}
	found := false
	for _, ct := range vc.ClickTracking {
		if ct.URI == click.ClickThrough {
			found = true
		}
	}
	if !found {
		t.Errorf("platform click URL must demote to ClickTracking when buyer owns ClickThrough: %+v", vc.ClickTracking)
	}

	// The injected document still marshals as 4.2.
	out, err := BuildDocument(doc.Ads)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if !strings.Contains(string(out), `version="4.2"`) || !strings.Contains(string(out), "brandx.example/imp") {
		t.Errorf("re-emitted doc lost version or buyer beacon:\n%s", out)
	}
}
