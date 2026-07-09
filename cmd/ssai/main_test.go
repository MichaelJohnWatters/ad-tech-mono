package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
)

// fakeTranscoder stands in for cmd/transcoder: on the serving (cache_only) call
// it returns a Conditioned ad of the given per-segment durations (default a 30s
// ad in five 6s segments), so the stitcher has real conditioned segments to
// splice — the on-disk ffmpeg path can't run in unit tests.
func fakeTranscoder(segDurs ...float64) *httptest.Server {
	if len(segDurs) == 0 {
		segDurs = []float64{6, 6, 6, 6, 6}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cache_only") != "1" {
			w.WriteHeader(http.StatusOK) // warm call
			return
		}
		var out transcode.Conditioned
		out.Cached = true
		for i, ds := range segDurs {
			out.Segments = append(out.Segments, transcode.CondSegment{
				URI: fmt.Sprintf("http://cdn.local/cond/seg_%d.ts", i), Duration: ds,
			})
			out.Duration += ds
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// replaySegments simulates the player fetching each stitched ad segment,
// firing its embedded server-side beacons (impression + quartiles).
func replaySegments(d *stitcherDeps, manifest string) {
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "/v1/ssai/seg?") {
			continue
		}
		d.segmentHandler(httptest.NewRecorder(), httptest.NewRequest("GET", line, nil))
	}
}

// TestManifestHandlerStitches asserts the end-to-end stitch: two breaks in the
// sample content each get filled from a stub SSP with conditioned ad segments,
// the content-during-break segments are replaced with SSAI segment beacon URLs,
// and the impression fires server-side — on ad-segment FETCH, not at manifest
// generation (so an unwatched mid/post-roll never books an impression).
func TestManifestHandlerStitches(t *testing.T) {
	var mu sync.Mutex
	var impressions, videoEvents int
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if strings.Contains(r.URL.Path, "/v1/t/imp") {
			impressions++
		}
		if strings.Contains(r.URL.Path, "/v1/t/video") {
			videoEvents++
		}
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer tracker.Close()

	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") != "video" {
			t.Errorf("SSAI called SSP with channel=%q, want video", r.URL.Query().Get("channel"))
		}
		// Forwarded viewer signal must survive into the auction.
		if r.URL.Query().Get("geo") != "GBR" {
			t.Errorf("SSAI dropped geo signal: %q", r.URL.Query().Get("geo"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sspWinner{
			TraceID: "trace-ssai", CreativeID: "cr-v", CampaignID: "li-v",
			PlacementID: "pl-sim-video", PublisherID: "pub", AdvertiserID: "adv",
			ClearingPrice: 5.0, Currency: "USD", DurationSeconds: 30,
			MediaURL: "https://cdn.example/ad.mp4",
		})
	}))
	defer ssp.Close()

	transcoder := fakeTranscoder() // 30s ad in five 6s conditioned segments
	defer transcoder.Close()

	d := &stitcherDeps{
		sspURL: ssp.URL, trackerURL: tracker.URL, publicURL: "http://pub.local",
		placementFn:   func() string { return "pl-sim-video" },
		transcoderURL: transcoder.URL,
		client:        &http.Client{},
	}

	req := httptest.NewRequest("GET", "/v1/ssai/manifest.m3u8?geo=GBR&device=ctv", nil)
	rec := httptest.NewRecorder()
	d.manifestHandler(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()

	// Original content-during-break segments (content_000, content_005) must be
	// replaced by ad segment beacon URLs.
	if strings.Contains(out, "content_000.ts") || strings.Contains(out, "content_005.ts") {
		t.Errorf("content-during-break not replaced:\n%s", out)
	}
	if !strings.Contains(out, "/v1/ssai/seg?") {
		t.Errorf("no SSAI segment beacon URLs in manifest:\n%s", out)
	}
	if !strings.Contains(out, "event=start") || !strings.Contains(out, "event=complete") {
		t.Errorf("quartile events missing from segment URLs:\n%s", out)
	}
	// The stitched output must still be a valid manifest.
	if _, err := ssai.ParseMedia(out); err != nil {
		t.Errorf("stitched manifest invalid: %v", err)
	}

	// No beacon fires from manifest generation alone — impressions are deferred
	// to segment fetch.
	mu.Lock()
	if impressions != 0 {
		t.Errorf("impression fired at manifest time (over-counting risk), got %d", impressions)
	}
	mu.Unlock()

	// Now the player fetches the segments: exactly one impression per filled
	// break (2), and all five quartiles per break fire (2×5 = 10 video events).
	replaySegments(d, out)
	mu.Lock()
	defer mu.Unlock()
	if impressions != 2 {
		t.Errorf("want 2 impressions (one per filled break, on first ad segment), got %d", impressions)
	}
	if videoEvents != 10 {
		t.Errorf("want 10 video events (5 quartiles × 2 breaks), got %d", videoEvents)
	}
}

// TestSegmentHandlerFiresBeaconAndRedirects asserts the per-segment endpoint
// fires the pre-signed quartile beacon verbatim (an HMAC-signed tracker URL —
// NOT a hand-rolled one) and 302s to the media.
func TestSegmentHandlerFiresBeaconAndRedirects(t *testing.T) {
	var got string
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.String()
		w.WriteHeader(200)
	}))
	defer tracker.Close()

	// Build the real signed beacon the stitcher would embed, so this test
	// proves the handler fires a validatable URL (passes signature_validation),
	// not a fake one.
	mc := macroCtxFor(&sspWinner{CampaignID: "li-x", CreativeID: "cr-x", PlacementID: "pl-x", PublisherID: "pub-x"}, "trace-x", tracker.URL)
	signed := adserving.BuildVideoEventURL(mc, "midpoint")

	d := &stitcherDeps{trackerURL: tracker.URL, client: &http.Client{}}
	u := "/v1/ssai/seg?ad=trace-x&event=midpoint&redir=https%3A%2F%2Fcdn.example%2Fad.mp4&beacon=" + url.QueryEscape(signed)
	req := httptest.NewRequest("GET", u, nil)
	rec := httptest.NewRecorder()
	d.segmentHandler(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://cdn.example/ad.mp4" {
		t.Errorf("redirect Location = %q", loc)
	}
	if !strings.Contains(got, "/v1/t/video") || !strings.Contains(got, "event=midpoint") {
		t.Errorf("beacon not fired through /v1/t/video with the event: %q", got)
	}
	// The fired beacon must carry a real HMAC sig, not sig=ssai.
	if !strings.Contains(got, "sig=") || strings.Contains(got, "sig=ssai") {
		t.Errorf("beacon must be HMAC-signed, not a hand-rolled sig: %q", got)
	}
}

func TestEventsForSegment(t *testing.T) {
	// Walk an ad of `total` seconds split into `segs`, flattening every event
	// each segment fires. All five VAST marks must fire exactly once, in order,
	// regardless of how the ad divides into segments.
	walk := func(total float64, segs []float64) []string {
		var all []string
		var start float64
		for _, d := range segs {
			all = append(all, eventsForSegment(start, d, total)...)
			start += d
		}
		return all
	}
	want := []string{"start", "firstQuartile", "midpoint", "thirdQuartile", "complete"}

	// 30s ad in five 6s segments: one mark per segment.
	if got := walk(30, []float64{6, 6, 6, 6, 6}); !equalStrs(got, want) {
		t.Errorf("30s/6s events = %v, want %v", got, want)
	}
	// 15s ad in 6/6/3 segments: some segments cross two marks — all five still
	// fire (the old one-event-per-segment logic dropped midpoint/thirdQuartile).
	if got := walk(15, []float64{6, 6, 3}); !equalStrs(got, want) {
		t.Errorf("15s/[6,6,3] events = %v, want %v", got, want)
	}
	// A single-segment ad still fires every mark.
	if got := walk(6, []float64{6}); !equalStrs(got, want) {
		t.Errorf("6s/[6] events = %v, want %v", got, want)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestOriginManifestFetched asserts the stitcher fetches a configured origin
// manifest (not the built-in sample) and stitches ads into ITS breaks.
func TestOriginManifestFetched(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, "#EXTM3U\n#EXTINF:6.0,\nmyorigin_001.ts\n#EXT-X-CUE-OUT:DURATION=6\n#EXTINF:6.0,\nmyorigin_002.ts\n#EXT-X-CUE-IN\n#EXTINF:6.0,\nmyorigin_003.ts\n#EXT-X-ENDLIST\n")
	}))
	defer origin.Close()

	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sspWinner{TraceID: "t", CreativeID: "cr", CampaignID: "li", MediaURL: "https://cdn/ad.mp4", DurationSeconds: 6})
	}))
	defer ssp.Close()

	transcoder := fakeTranscoder(6) // one 6s conditioned ad segment
	defer transcoder.Close()

	d := &stitcherDeps{
		sspURL: ssp.URL, trackerURL: "http://tracker", publicURL: "http://pub",
		placementFn:   func() string { return "pl" },
		originFn:      func() string { return origin.URL }, // <- real origin
		transcoderURL: transcoder.URL,
		client:        &http.Client{},
	}
	rec := httptest.NewRecorder()
	d.manifestHandler(rec, httptest.NewRequest("GET", "/v1/ssai/manifest.m3u8", nil))
	out := rec.Body.String()

	if strings.Contains(out, "content_000.ts") {
		t.Errorf("stitched the built-in sample, not the fetched origin:\n%s", out)
	}
	if !strings.Contains(out, "myorigin_001.ts") || !strings.Contains(out, "myorigin_003.ts") {
		t.Errorf("origin content segments missing:\n%s", out)
	}
	if strings.Contains(out, "myorigin_002.ts") {
		t.Errorf("origin break content not replaced by ad:\n%s", out)
	}
	if !strings.Contains(out, "/v1/ssai/seg?") {
		t.Errorf("no ad stitched into the origin break:\n%s", out)
	}
}
