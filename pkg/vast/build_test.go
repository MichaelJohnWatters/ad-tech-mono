package vast

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

// Builds a minimal linear video VAST and asserts the document
// round-trips through encoding/xml — the most expensive thing the
// test catches is a struct tag drift (the wrong xml:"..." attr name
// would silently produce a non-spec document players reject).
func TestBuildLinearAd_Video_Roundtrips(t *testing.T) {
	spec := LinearSpec{
		AdID:       "ad-1",
		AdSystem:   "ad-tech-mono",
		AdTitle:    "LuxAuto Pre-Roll",
		Advertiser: "luxauto.com",
		Duration:   15 * time.Second,
		MediaFiles: []MediaFile{{
			Delivery: "progressive",
			Type:     "video/mp4",
			Bitrate:  800,
			Width:    640,
			Height:   360,
			URI:      "https://cdn.example/luxauto-15s.mp4",
		}},
		Trackers: LinearTrackers{
			Impression:    []string{"https://t.example/imp?tid=x"},
			Start:         []string{"https://t.example/start?tid=x"},
			FirstQuartile: []string{"https://t.example/q1?tid=x"},
			Midpoint:      []string{"https://t.example/mid?tid=x"},
			ThirdQuartile: []string{"https://t.example/q3?tid=x"},
			Complete:      []string{"https://t.example/done?tid=x"},
			Progress: []ProgressTracker{
				{Offset: 5 * time.Second, URL: "https://t.example/5s?tid=x"},
			},
		},
		Click: ClickSpec{
			ClickThrough:  "https://t.example/click?tid=x",
			ClickTracking: []string{"https://t.example/click-px?tid=x"},
		},
	}

	xmlBytes, err := BuildLinearAd(spec)
	if err != nil {
		t.Fatalf("BuildLinearAd: %v", err)
	}
	s := string(xmlBytes)
	for _, want := range []string{
		`xsi:noNamespaceSchemaLocation="vast.xsd"`,
		`version="4.2"`,
		`<Ad id="ad-1">`,
		`<AdSystem version="1.0">ad-tech-mono</AdSystem>`,
		`<UniversalAdId idRegistry="ad-tech-mono">ad-1</UniversalAdId>`,
		`<Duration>00:00:15</Duration>`,
		`event="firstQuartile"`,
		`event="progress" offset="00:00:05"`,
		`type="video/mp4"`,
		`bitrate="800"`,
		`width="640"`,
		`<ClickThrough>`,
		`luxauto-15s.mp4`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in VAST output:\n%s", want, s)
		}
	}

	var decoded VAST
	if err := xml.Unmarshal(xmlBytes, &decoded); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if decoded.Version != Version || len(decoded.Ads) != 1 {
		t.Fatalf("decoded shape wrong: %+v", decoded)
	}
	lin := decoded.Ads[0].InLine.Creatives.Creatives[0].Linear
	if lin == nil || len(lin.MediaFiles.MediaFiles) != 1 {
		t.Fatalf("media file lost in round-trip")
	}
	if mf := lin.MediaFiles.MediaFiles[0]; mf.Type != "video/mp4" || mf.Width != 640 {
		t.Errorf("media file fields wrong: %+v", mf)
	}
}

// Audio variant: same shape, audio MIME, no width/height. Verifies the
// builder omits the dimension attributes when they're zero (otherwise
// every audio MediaFile would carry width="0" height="0" which trips
// strict players).
func TestBuildLinearAd_OMIDVerifications(t *testing.T) {
	spec := LinearSpec{
		AdID:       "ad-omid",
		AdSystem:   "ad-tech-mono",
		AdTitle:    "Verified Pre-Roll",
		Advertiser: "acme.com",
		Duration:   15 * time.Second,
		MediaFiles: []MediaFile{{Delivery: "progressive", Type: "video/mp4", URI: "https://cdn/x.mp4"}},
		Verifications: []OMIDVerification{{
			Vendor:         "measure.example-omid",
			ScriptURL:      "https://measure.example/omweb-v1.js",
			Parameters:     `{"k":"v"}`,
			NotExecutedURL: "https://measure.example/notexec?tid=x",
		}},
	}
	out, err := BuildLinearAd(spec)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"<AdVerifications>",
		`vendor="measure.example-omid"`,
		`apiFramework="omid"`,
		"omweb-v1.js",
		`event="verificationNotExecuted"`,
		"<VerificationParameters>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("VAST missing %q\n%s", want, s)
		}
	}

	// Must still round-trip through the parser.
	var doc VAST
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	av := doc.Ads[0].InLine.AdVerifications
	if av == nil || len(av.Verifications) != 1 {
		t.Fatalf("expected 1 verification, got %+v", av)
	}
	if av.Verifications[0].JavaScriptResource.APIFramework != "omid" {
		t.Errorf("apiFramework = %q, want omid", av.Verifications[0].JavaScriptResource.APIFramework)
	}
}

func TestBuildLinearAd_NoVerifications_OmitsElement(t *testing.T) {
	spec := LinearSpec{
		AdID:       "ad-plain",
		AdTitle:    "Plain",
		Duration:   10 * time.Second,
		MediaFiles: []MediaFile{{Delivery: "progressive", Type: "video/mp4", URI: "https://cdn/x.mp4"}},
	}
	out, _ := BuildLinearAd(spec)
	if strings.Contains(string(out), "AdVerifications") {
		t.Errorf("expected no AdVerifications element when none set:\n%s", out)
	}
}

func TestBuildLinearAd_AudioOnly_OmitsDimensions(t *testing.T) {
	spec := LinearSpec{
		AdID:       "audio-1",
		AdTitle:    "VPN Podcast Spot",
		Advertiser: "vpnplus.example.com",
		Duration:   30 * time.Second,
		MediaFiles: []MediaFile{{
			Delivery: "progressive",
			Type:     "audio/mpeg",
			Bitrate:  128,
			URI:      "https://cdn.example/vpn-30s.mp3",
		}},
		Trackers: LinearTrackers{
			Impression: []string{"https://t.example/imp?tid=x"},
			Complete:   []string{"https://t.example/done?tid=x"},
		},
	}

	if !IsAudioOnly(spec) {
		t.Error("IsAudioOnly should be true for audio/mpeg-only spec")
	}

	xmlBytes, err := BuildLinearAd(spec)
	if err != nil {
		t.Fatalf("BuildLinearAd: %v", err)
	}
	s := string(xmlBytes)
	if strings.Contains(s, "width=") || strings.Contains(s, "height=") {
		t.Errorf("audio MediaFile should not carry width/height:\n%s", s)
	}
	if !strings.Contains(s, `type="audio/mpeg"`) {
		t.Errorf("audio MIME missing")
	}
}

// Pod build: three sequenced ads in one document. Verifies the
// Sequence attribute and that BuildPod auto-numbers entries with
// Sequence == 0 in their input position.
func TestBuildPod_Sequences(t *testing.T) {
	specs := []LinearSpec{
		{AdID: "a", AdTitle: "spot 1", Duration: 15 * time.Second,
			MediaFiles: []MediaFile{{Delivery: "progressive", Type: "video/mp4", URI: "https://x/1.mp4"}}},
		{AdID: "b", AdTitle: "spot 2", Duration: 15 * time.Second,
			MediaFiles: []MediaFile{{Delivery: "progressive", Type: "video/mp4", URI: "https://x/2.mp4"}}},
		{AdID: "c", AdTitle: "spot 3", Duration: 30 * time.Second,
			MediaFiles: []MediaFile{{Delivery: "progressive", Type: "video/mp4", URI: "https://x/3.mp4"}}},
	}
	xmlBytes, err := BuildPod(specs)
	if err != nil {
		t.Fatalf("BuildPod: %v", err)
	}
	var decoded VAST
	if err := xml.Unmarshal(xmlBytes, &decoded); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(decoded.Ads) != 3 {
		t.Fatalf("expected 3 ads, got %d", len(decoded.Ads))
	}
	for i, ad := range decoded.Ads {
		want := i + 1
		if ad.Sequence != want {
			t.Errorf("ad[%d].Sequence = %d, want %d", i, ad.Sequence, want)
		}
	}
}

// Duration formatting: 0, sub-second, whole-second, hour boundaries.
// VAST players are strict — a malformed duration field gets the ad
// rejected.
func TestDuration_MarshalText(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "00:00:00"},
		{15 * time.Second, "00:00:15"},
		{90 * time.Second, "00:01:30"},
		{3661 * time.Second, "01:01:01"},
		{500 * time.Millisecond, "00:00:00.500"},
	}
	for _, c := range cases {
		got, _ := Duration(c.in).MarshalText()
		if string(got) != c.want {
			t.Errorf("MarshalText(%v) = %q, want %q", c.in, string(got), c.want)
		}
	}
}

// CDATA fields keep query-string &-chars unescaped — players that
// strip the CDATA wrapper get the URL back verbatim.
func TestBuildLinearAd_CDATA_KeepsAmpersands(t *testing.T) {
	spec := LinearSpec{
		AdID:     "amp-1",
		AdTitle:  "test",
		Duration: 5 * time.Second,
		MediaFiles: []MediaFile{{
			Delivery: "progressive",
			Type:     "video/mp4",
			URI:      "https://x/v.mp4?a=1&b=2",
		}},
		Trackers: LinearTrackers{Impression: []string{"https://t/imp?a=1&b=2"}},
	}
	xmlBytes, _ := BuildLinearAd(spec)
	s := string(xmlBytes)
	if !strings.Contains(s, "https://x/v.mp4?a=1&b=2") {
		t.Errorf("ampersand was escaped inside CDATA:\n%s", s)
	}
	if !strings.Contains(s, "<![CDATA[https://t/imp?a=1&b=2]]>") {
		t.Errorf("impression URL not wrapped in CDATA:\n%s", s)
	}
}
