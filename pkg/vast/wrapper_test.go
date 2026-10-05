package vast

import (
	"strings"
	"testing"
)

// TestBuildWrapperAd_RoundTrips locks the wrapper shape: behaviour attrs
// (spec defaults true/false/true), CDATA VASTAdTagURI, wrapper-level
// Impression/Error, wrapper tracking creative WITHOUT Duration/MediaFiles —
// and that the document parses back through our own Parse.
func TestBuildWrapperAd_RoundTrips(t *testing.T) {
	xmlBytes, err := BuildWrapperAd(WrapperSpec{
		AdID:         "wrap-1",
		AdSystem:     "ad-tech-mono-dsp",
		VASTAdTagURI: "http://localhost:8080/v1/pubad/video/vast?placement_id=third-party",
		Trackers: LinearTrackers{
			Impression: []string{"http://t/v1/t/imp?tid=x&sig=s"},
			Start:      []string{"http://t/v1/t/video?event=start&sig=s"},
		},
		ErrorURLs: []string{"http://t/v1/t/video?event=error&sig=s&ec=[ERRORCODE]"},
	})
	if err != nil {
		t.Fatalf("BuildWrapperAd: %v", err)
	}
	s := string(xmlBytes)
	for _, want := range []string{
		`followAdditionalWrappers="true"`,
		`allowMultipleAds="false"`,
		`fallbackOnNoAd="true"`,
		"<VASTAdTagURI><![CDATA[http://localhost:8080/v1/pubad/video/vast?placement_id=third-party]]></VASTAdTagURI>",
		"ec=[ERRORCODE]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wrapper XML missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "<Duration>") || strings.Contains(s, "<MediaFiles>") {
		t.Errorf("wrapper tracking creative must carry no Duration/MediaFiles:\n%s", s)
	}

	doc, err := Parse(xmlBytes)
	if err != nil {
		t.Fatalf("own wrapper output must parse back: %v", err)
	}
	w := doc.Ads[0].Wrapper
	if w == nil || w.VASTAdTagURI.URI == "" || len(w.Impressions) != 1 || len(w.Errors) != 1 {
		t.Fatalf("round-trip lost wrapper shape: %+v", w)
	}
	if w.Creatives == nil || w.Creatives.Creatives[0].Linear == nil ||
		w.Creatives.Creatives[0].Linear.TrackingEvents == nil {
		t.Fatalf("wrapper tracking creative lost: %+v", w.Creatives)
	}
}

// TestInjectLinearTrackers_Wrapper: injecting into a bare DSP wrapper (no
// trackers) adds platform impressions/errors at wrapper level and a wrapper
// tracking creative, sets behaviour-attr defaults, and stays idempotent.
func TestInjectLinearTrackers_Wrapper(t *testing.T) {
	xmlBytes, err := BuildWrapperAd(WrapperSpec{AdID: "wrap-2", VASTAdTagURI: "http://tag.example/vast"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	doc, err := Parse(xmlBytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	platform := LinearTrackers{
		Impression: []string{"http://t/v1/t/imp?tid=x&sig=s"},
		Start:      []string{"http://t/v1/t/video?event=start&sig=s"},
		Complete:   []string{"http://t/v1/t/video?event=complete&sig=s"},
	}
	errs := []string{"http://t/v1/t/video?event=error&sig=s&ec=[ERRORCODE]"}
	click := ClickSpec{ClickThrough: "http://t/v1/t/click?tid=x&sig=s"}

	doc.InjectLinearTrackers(platform, errs, click)
	doc.InjectLinearTrackers(platform, errs, click) // idempotent

	w := doc.Ads[0].Wrapper
	if len(w.Impressions) != 1 || len(w.Errors) != 1 {
		t.Fatalf("wrapper-level inject wrong: imps=%d errs=%d", len(w.Impressions), len(w.Errors))
	}
	if w.FollowAdditionalWrappers != "true" || w.AllowMultipleAds != "false" || w.FallbackOnNoAd != "true" {
		t.Errorf("behaviour attrs not defaulted: %+v", w)
	}
	lin := w.Creatives.Creatives[0].Linear
	if lin == nil || lin.TrackingEvents == nil || len(lin.TrackingEvents.Tracking) != 2 {
		t.Fatalf("wrapper tracking creative wrong: %+v", lin)
	}
	if lin.VideoClicks == nil || lin.VideoClicks.ClickThrough == nil {
		t.Fatalf("wrapper click-through not injected: %+v", lin.VideoClicks)
	}
}
