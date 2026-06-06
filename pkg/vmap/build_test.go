package vmap

import (
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// TimeOffset.Format covers the four shapes VMAP accepts. A drift in
// any of these would silently produce a malformed timeOffset attribute
// and players would skip the break.
func TestTimeOffset_Format(t *testing.T) {
	cases := []struct {
		in   TimeOffset
		want string
	}{
		{TimeOffset{Start: true}, "start"},
		{TimeOffset{End: true}, "end"},
		{TimeOffset{AbsoluteAt: 5 * time.Minute}, "00:05:00"},
		{TimeOffset{AbsoluteAt: time.Hour + 30*time.Second}, "01:00:30"},
		{TimeOffset{AbsoluteAt: 1500 * time.Millisecond}, "00:00:01.500"},
		{TimeOffset{PercentOf: 25}, "25%"},
		{TimeOffset{}, "start"}, // zero value → "start"
	}
	for _, c := range cases {
		if got := c.in.Format(); got != c.want {
			t.Errorf("Format(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// End-to-end: build a pre-roll + mid-roll + post-roll schedule, each
// carrying an inline VAST document, and assert the resulting XML
// carries the right shape. The inline-VAST case is the more important
// one — VMAP players parse the VAST out via their own VAST parser, so
// the surrounding VMAP must keep the document intact (no escaping of
// the < > characters, no whitespace stripping that loses CDATA).
func TestBuildSchedule_PreMidPost(t *testing.T) {
	mkVAST := func(adID, title string, dur time.Duration) []byte {
		out, err := vast.BuildLinearAd(vast.LinearSpec{
			AdID: adID, AdTitle: title, Duration: dur,
			MediaFiles: []vast.MediaFile{{
				Delivery: "progressive", Type: "video/mp4",
				URI: "https://x/" + adID + ".mp4",
			}},
			Trackers: vast.LinearTrackers{Impression: []string{"https://t/imp?ad=" + adID}},
		})
		if err != nil {
			t.Fatalf("vast build: %v", err)
		}
		return out
	}

	specs := []BreakSpec{
		PreRoll("pre", mkVAST("preroll", "Pre-roll spot", 15*time.Second),
			BreakTrackers{
				BreakStart: []string{"https://t/break-start?br=pre"},
				BreakEnd:   []string{"https://t/break-end?br=pre"},
			}),
		MidRoll("mid-1", TimeOffset{AbsoluteAt: 5 * time.Minute},
			mkVAST("midroll1", "Mid-roll 1", 30*time.Second),
			BreakTrackers{Error: []string{"https://t/break-err?br=mid-1"}}),
		PostRoll("post", mkVAST("postroll", "Post-roll spot", 15*time.Second),
			BreakTrackers{}),
	}

	out, err := BuildSchedule(specs)
	if err != nil {
		t.Fatalf("BuildSchedule: %v", err)
	}
	s := string(out)

	for _, want := range []string{
		`<vmap:VMAP xmlns:vmap="http://www.iab.net/videosuite/vmap" version="1.0">`,
		`<vmap:AdBreak breakType="linear" timeOffset="start" breakId="pre">`,
		`<vmap:AdBreak breakType="linear" timeOffset="00:05:00" breakId="mid-1">`,
		`<vmap:AdBreak breakType="linear" timeOffset="end" breakId="post">`,
		`<vmap:AdSource id="pre"`,
		`<vmap:VASTAdData>`,
		`<VAST version="4.2">`, // inline VAST survives
		`<Ad id="preroll">`,
		`<Ad id="midroll1">`,
		`<vmap:Tracking event="breakStart"><![CDATA[https://t/break-start?br=pre]]></vmap:Tracking>`,
		`<vmap:Tracking event="error"><![CDATA[https://t/break-err?br=mid-1]]></vmap:Tracking>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in VMAP output:\n%s", want, s)
		}
	}
}

// The inline VAST must not carry a nested XML declaration when embedded
// inside a VMAP — strict parsers reject the whole document. stripXMLDecl
// in the builder takes care of this; the test pins it down.
func TestBuildSchedule_InlineVASTHasNoNestedXMLDecl(t *testing.T) {
	v, _ := vast.BuildLinearAd(vast.LinearSpec{
		AdID: "x", AdTitle: "x", Duration: 5 * time.Second,
		MediaFiles: []vast.MediaFile{{Delivery: "progressive", Type: "video/mp4", URI: "https://x/v.mp4"}},
	})
	out, _ := BuildSchedule([]BreakSpec{PreRoll("p", v, BreakTrackers{})})
	// The single top-of-document XML declaration is fine.
	if !strings.HasPrefix(strings.TrimSpace(string(out)), `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Errorf("VMAP must start with XML declaration:\n%s", out)
	}
	// But there should be exactly one — count occurrences.
	if c := strings.Count(string(out), "<?xml"); c != 1 {
		t.Errorf("found %d <?xml declarations in VMAP, want 1:\n%s", c, out)
	}
}

// AdTagURI shape: deferred-fetch break where the player will call back
// at break time. Verifies the templateType attribute and that the URL
// gets CDATA-wrapped (matches inline-VAST URL handling).
func TestBuildSchedule_AdTagURI(t *testing.T) {
	follow := false
	specs := []BreakSpec{{
		BreakID:          "pre",
		Offset:           TimeOffset{Start: true},
		AdTagURL:         "https://exchange.example/vmap/pod-pre?u=$USER",
		AdTagTemplate:    "vast4.2",
		FollowRedirects:  &follow,
		AllowMultipleAds: true,
	}}
	out, err := BuildSchedule(specs)
	if err != nil {
		t.Fatalf("BuildSchedule: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`<vmap:AdSource id="pre" allowMultipleAds="true" followRedirects="false">`,
		`<vmap:AdTagURI templateType="vast4.2"><![CDATA[https://exchange.example/vmap/pod-pre?u=$USER]]></vmap:AdTagURI>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in VMAP output:\n%s", want, s)
		}
	}
}

// Empty schedule (no breaks) is valid — represents "no ads in this
// content"; players that fetch the VMAP just play through.
func TestBuildSchedule_Empty(t *testing.T) {
	out, err := BuildSchedule(nil)
	if err != nil {
		t.Fatalf("BuildSchedule: %v", err)
	}
	if !strings.Contains(string(out), `<vmap:VMAP xmlns:vmap="http://www.iab.net/videosuite/vmap" version="1.0">`) {
		t.Errorf("empty schedule missing VMAP root:\n%s", out)
	}
	if strings.Contains(string(out), "AdBreak") {
		t.Errorf("empty schedule should not contain AdBreak:\n%s", out)
	}
}
