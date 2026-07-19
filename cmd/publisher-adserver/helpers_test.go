package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
)

// noHouseAds is a houseAdLookup that never finds a configured house ad — so a
// no-bid with the fallback on still yields an honest no-fill. Used by tests
// that either fill from a real auction or assert the no-content path.
func noHouseAds(string, uint64) (houseads.HouseAd, bool) { return houseads.HouseAd{}, false }

// houseAdFrom builds a houseAdLookup backed by a fixed set of house ads (via the
// same weighted Pick the ad server uses). Lets a no-bid test seed an ops-defined
// house ad and assert the ad server serves ITS markup.
func houseAdFrom(ads ...houseads.HouseAd) houseAdLookup {
	return func(format string, seed uint64) (houseads.HouseAd, bool) {
		return houseads.Pick(ads, format, seed)
	}
}

// houseVideoVAST / houseAudioVAST / houseNativeHTML are valid markup fixtures a
// staff member could paste into a house ad for each format. The video/audio
// markup is inline VAST XML (served verbatim); the native markup is an HTML card.
const houseVideoVAST = `<?xml version="1.0" encoding="UTF-8"?>
<VAST version="4.2"><Ad id="house-video"><InLine><AdSystem>ad-tech-mono</AdSystem><AdTitle>House Video</AdTitle>` +
	`<Impression><![CDATA[http://tracker:8083/v1/t/imp?house=1]]></Impression>` +
	`<Creatives><Creative><Linear><Duration>00:00:15</Duration>` +
	`<MediaFiles><MediaFile delivery="progressive" type="video/mp4" width="640" height="360"><![CDATA[http://cdn/house.mp4]]></MediaFile></MediaFiles>` +
	`</Linear></Creative></Creatives></InLine></Ad></VAST>`

const houseAudioVAST = `<?xml version="1.0" encoding="UTF-8"?>
<VAST version="4.2"><Ad id="house-audio"><InLine><AdSystem>ad-tech-mono</AdSystem><AdTitle>House Audio</AdTitle>` +
	`<Impression><![CDATA[http://tracker:8083/v1/t/imp?house=1]]></Impression>` +
	`<Creatives><Creative><Linear><Duration>00:00:30</Duration>` +
	`<MediaFiles><MediaFile delivery="progressive" type="audio/mpeg"><![CDATA[http://cdn/house.mp3]]></MediaFile></MediaFiles>` +
	`</Linear></Creative></Creatives></InLine></Ad></VAST>`

const houseNativeHTML = `<div class="house-native">Try AdTech Mono — the transparent ad platform</div>`

// houseVideoFn is a houseAdLookup with a single enabled video house ad — for
// tests that assert a no-bid serves the configured house ad's VAST markup.
func houseVideoFn() houseAdLookup {
	return houseAdFrom(houseads.HouseAd{
		ID: "22222222-2222-4222-8222-222222222222", Format: houseads.FormatVideo,
		Name: "House Video", Markup: houseVideoVAST, Enabled: true, Weight: 1,
	})
}

func TestDeviceFromUserAgent(t *testing.T) {
	cases := map[string]string{
		// iPad is intentionally checked BEFORE the "Mobile" substring so
		// iOS tablet UAs (which contain both "iPad" and "Mobile") land
		// on tablet, not mobile. The implementation comment notes this.
		"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Mobile/15E148": "tablet",
		"Mozilla/5.0 (Linux; Android 13; Pixel 7) Mobile":             "mobile",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)":      "mobile",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)":                   "desktop",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0)":                "desktop",
		"":                                 "desktop", // empty UA → safe default
		"curl/8.4.0":                       "desktop", // CLI tools → desktop
		"Mozilla/5.0 (compatible; Tablet)": "tablet",
	}
	for ua, want := range cases {
		if got := deviceFromUserAgent(ua); got != want {
			t.Errorf("deviceFromUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
}

// writeNoBid is the canonical no-fill response shape every no-bid path
// in publisher-adserver routes through. Pin the field set + reason
// passthrough so future paths don't accidentally drop the reason
// (which the SDK + simulator branch on).
func TestWriteNoBid_ResponseShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeNoBid(rec, "trace-abc", "freqcap")

	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, rec.Body.String())
	}
	if body["trace_id"] != "trace-abc" {
		t.Errorf("trace_id = %v", body["trace_id"])
	}
	if body["no_fill"] != true {
		t.Errorf("no_fill = %v, want true", body["no_fill"])
	}
	if body["reason"] != "freqcap" {
		t.Errorf("reason = %v, want freqcap", body["reason"])
	}
	// Legacy "nobid" field still present for older e2e fixtures —
	// pinned so the cleanup doesn't accidentally land before the
	// fixtures are migrated.
	if body["nobid"] != true {
		t.Errorf("legacy nobid field missing; older e2e fixtures expect it: %v", body)
	}
	if body["source"] != "none" {
		t.Errorf("source = %v, want none", body["source"])
	}
}

// Sanity: HTTP status remains 200 even though the response says no
// fill. The body shape is the signal, not the status code — keeps the
// SDK from branching on transport errors for what is a logical
// "auction ran, nobody bid" case.
func TestWriteNoBid_Returns200(t *testing.T) {
	rec := httptest.NewRecorder()
	writeNoBid(rec, "trace-abc", "no_eligible_campaigns")
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
