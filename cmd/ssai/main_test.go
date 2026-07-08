package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
)

// TestManifestHandlerStitches asserts the end-to-end stitch: two breaks in the
// sample content each get filled from a stub SSP, the content-during-break
// segments are replaced with SSAI segment beacon URLs, and the impression
// beacon fires server-side.
func TestManifestHandlerStitches(t *testing.T) {
	var mu sync.Mutex
	var impressions int
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/t/imp") {
			mu.Lock()
			impressions++
			mu.Unlock()
		}
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

	d := &stitcherDeps{
		sspURL: ssp.URL, trackerURL: tracker.URL, publicURL: "http://pub.local",
		segDurFn: func() float64 { return 6 }, placementFn: func() string { return "pl-sim-video" },
		client: &http.Client{},
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

	mu.Lock()
	defer mu.Unlock()
	if impressions != 2 {
		t.Errorf("want 2 server-side impressions (one per filled break), got %d", impressions)
	}
}

// TestSegmentHandlerFiresBeaconAndRedirects asserts the per-segment endpoint
// fires the quartile beacon server-side and 302s to the media.
func TestSegmentHandlerFiresBeaconAndRedirects(t *testing.T) {
	var got string
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.String()
		w.WriteHeader(200)
	}))
	defer tracker.Close()

	d := &stitcherDeps{trackerURL: tracker.URL, client: &http.Client{}}
	req := httptest.NewRequest("GET", "/v1/ssai/seg?ad=trace-x&event=midpoint&redir=https://cdn.example/ad.mp4", nil)
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
}

func TestQuartileForSegment(t *testing.T) {
	// 30s ad, 6s segments → segment start offsets 0,6,12,18,24.
	if got := quartileForSegment(0, 30, 6); got != "start" {
		t.Errorf("seg@0 = %q, want start", got)
	}
	if got := quartileForSegment(24, 30, 6); got != "complete" {
		t.Errorf("seg@24 (last) = %q, want complete", got)
	}
	if got := quartileForSegment(18, 30, 6); got != "midpoint" && got != "thirdQuartile" {
		t.Errorf("seg@18 = %q, want a mid/third quartile", got)
	}
}
