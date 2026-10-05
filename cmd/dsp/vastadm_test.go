package main

import (
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// TestBuildMediaAdM: the DSP's video adm is a parseable VAST 4.2 InLine with
// the creative MediaFile and UniversalAdId, and — by decision — NO tracking
// beacons and NO Pricing (platform truth is the AuctionWinEvent; the
// publisher-adserver injects the platform's signed trackers at serve time).
func TestBuildMediaAdM(t *testing.T) {
	c := &models.Campaign{Name: "Lux Auto Q4", CreativeDomain: "luxauto.com"}
	bid := &openrtb.BidObj{CrID: "cr-1", W: 640, H: 360, Dur: 15, MediaURL: "http://minio:9000/creatives/lux.mp4"}

	adm, ok := buildMediaAdM(c, bid, "video")
	if !ok {
		t.Fatal("buildMediaAdM returned !ok for a valid video bid")
	}
	doc, err := vast.Parse([]byte(adm))
	if err != nil {
		t.Fatalf("DSP adm must parse as VAST: %v", err)
	}
	in := doc.Ads[0].InLine
	if in == nil || in.AdSystem.Name != "ad-tech-mono-dsp" {
		t.Fatalf("want InLine from ad-tech-mono-dsp, got %+v", doc.Ads[0])
	}
	if len(in.Impressions) != 0 || in.Pricing != nil {
		t.Errorf("DSP adm must carry no beacons/pricing (platform injects): imps=%d pricing=%v", len(in.Impressions), in.Pricing)
	}
	cr := in.Creatives.Creatives[0]
	if cr.UniversalAdID == nil || cr.UniversalAdID.Value != "cr-1" {
		t.Errorf("UniversalAdId must carry the creative id: %+v", cr.UniversalAdID)
	}
	mf := cr.Linear.MediaFiles.MediaFiles[0]
	if mf.Type != "video/mp4" || mf.Width != 640 || !strings.Contains(mf.URI, "lux.mp4") {
		t.Errorf("MediaFile wrong: %+v", mf)
	}

	// Audio: audio MIME, no dimensions.
	abid := &openrtb.BidObj{CrID: "cr-a", Dur: 30, MediaURL: "http://minio:9000/creatives/spot.mp3"}
	adm, ok = buildMediaAdM(c, abid, "audio")
	if !ok {
		t.Fatal("audio adm !ok")
	}
	doc, err = vast.Parse([]byte(adm))
	if err != nil {
		t.Fatalf("audio adm parse: %v", err)
	}
	amf := doc.Ads[0].InLine.Creatives.Creatives[0].Linear.MediaFiles.MediaFiles[0]
	if amf.Type != "audio/mpeg" || amf.Width != 0 {
		t.Errorf("audio MediaFile wrong: %+v", amf)
	}

	// No media URL → no adm (bid still valid via the legacy path).
	if _, ok := buildMediaAdM(c, &openrtb.BidObj{CrID: "x"}, "video"); ok {
		t.Error("empty MediaURL must not produce adm")
	}
}

func TestMediaMIME(t *testing.T) {
	cases := map[[2]string]string{
		{"http://cdn/x.mp4", "video"}:      "video/mp4",
		{"http://cdn/x.webm?v=1", "video"}: "video/webm",
		{"http://cdn/x.mp3", "audio"}:      "audio/mpeg",
		{"http://cdn/x.m4a", "audio"}:      "audio/mp4",
		{"http://cdn/noext", "audio"}:      "audio/mpeg",
		{"http://cdn/noext", "video"}:      "video/mp4",
	}
	for in, want := range cases {
		if got := mediaMIME(in[0], in[1]); got != want {
			t.Errorf("mediaMIME(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
