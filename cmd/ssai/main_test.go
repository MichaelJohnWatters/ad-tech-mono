package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/dash"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssai"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
)

// TestStitchWithRealConditioner is the R1 HTTP-path proof: the real stitcher
// against a real ffmpeg conditioner (fs object store, no cluster). It conditions
// a genuine clip, then asserts the stitched manifest points each ad segment at a
// real conditioned .ts. Auto-skips without ffmpeg.
func TestStitchWithRealConditioner(t *testing.T) {
	r := transcode.Runner{}
	if !r.Available() {
		t.Skip("ffmpeg not installed; skipping real-conditioner stitch")
	}
	ctx := context.Background()

	// Object store + a real mezzanine seeded under the /v1/creatives key space.
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bucket := "adtech-creatives"
	if err := store.EnsureBucket(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "spot.mp4")
	gen := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=6:size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6", "-c:v", "libx264", "-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v\n%s", err, out)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, bucket, "media/spot.mp4", strings.NewReader(string(b)), int64(len(b)), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	mediaURL := "http://host/v1/creatives/media/spot.mp4"

	cond := &transcode.Conditioner{Store: store, Runner: r, Bucket: bucket,
		Prefix: "ssai/cond", PublicBase: "http://pub.local/v1/creatives"}
	// Pre-warm the cache so the stitcher's cache-only lookup hits deterministically.
	if _, err := cond.Condition(ctx, "cr-v", mediaURL, transcode.DefaultProfile()); err != nil {
		t.Fatalf("pre-warm condition: %v", err)
	}

	// A real transcoder HTTP endpoint backed by the conditioner.
	transcoder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, hr *http.Request) {
		var req struct {
			CreativeID string            `json:"creative_id"`
			MediaURL   string            `json:"media_url"`
			Profile    transcode.Profile `json:"profile"`
		}
		_ = json.NewDecoder(hr.Body).Decode(&req)
		if req.Profile.Zero() {
			req.Profile = transcode.DefaultProfile()
		}
		if hr.URL.Query().Get("cache_only") == "1" {
			out, _ := cond.Cached(hr.Context(), req.CreativeID, req.MediaURL, req.Profile)
			if out == nil {
				http.Error(w, "not conditioned", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		out, err := cond.Condition(hr.Context(), req.CreativeID, req.MediaURL, req.Profile)
		if err != nil {
			http.Error(w, "fail", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer transcoder.Close()

	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, hr *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sspWinner{
			TraceID: "trace-real", CreativeID: "cr-v", CampaignID: "li-v",
			PlacementID: "pl", PublisherID: "pub", ClearingPrice: 5, Currency: "USD",
			MediaURL: mediaURL, DurationSeconds: 6,
		})
	}))
	defer ssp.Close()

	d := &stitcherDeps{
		sspURL: ssp.URL, trackerURL: "http://tracker", publicURL: "http://pub.local",
		placementFn:   func() string { return "pl" },
		transcoderURL: transcoder.URL,
		maxPodAdsFn:   func() int { return 1 },
		client:        &http.Client{},
	}

	rec := httptest.NewRecorder()
	d.manifestHandler(rec, httptest.NewRequest("GET", "/v1/ssai/manifest.m3u8", nil))
	out := rec.Body.String()

	if strings.Contains(out, "content_000.ts") {
		t.Errorf("content-during-break not replaced by real conditioned ad:\n%s", out)
	}
	if !strings.Contains(out, "/v1/ssai/seg?") {
		t.Fatalf("no ad segments stitched:\n%s", out)
	}
	if _, err := ssai.ParseMedia(out); err != nil {
		t.Errorf("stitched manifest invalid: %v", err)
	}

	// Follow the first ad segment: its redirect must resolve to a real conditioned
	// .ts under ssai/cond (proof the stitcher wired real conditioned output, not a
	// slate or the raw mezzanine).
	var segLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "/v1/ssai/seg?") {
			segLine = strings.TrimSpace(line)
			break
		}
	}
	segRec := httptest.NewRecorder()
	d.segmentHandler(segRec, httptest.NewRequest("GET", segLine, nil))
	if segRec.Code != http.StatusFound {
		t.Fatalf("segment endpoint status = %d, want 302", segRec.Code)
	}
	loc := segRec.Header().Get("Location")
	if !strings.Contains(loc, "ssai/cond/cr-v") || !strings.HasSuffix(loc, ".ts") {
		t.Errorf("ad segment redirect not a real conditioned .ts: %q", loc)
	}
}

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

// TestAdPodFillsMultipleAds asserts ad-pod filling: a 30s avail is filled with
// two back-to-back conditioned ads (not one), so across the sample's two breaks
// three ads run — proving the pod loop, distinct-creative dedup, and per-ad
// impressions.
func TestAdPodFillsMultipleAds(t *testing.T) {
	var mu sync.Mutex
	var impressions int
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if strings.Contains(r.URL.Path, "/v1/t/imp") {
			impressions++
		}
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer tracker.Close()

	var call int
	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		call++
		n := call
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Distinct creative + trace per call so the pod dedup doesn't stop early.
		_ = json.NewEncoder(w).Encode(sspWinner{
			TraceID: fmt.Sprintf("t-%d", n), CreativeID: fmt.Sprintf("cr-%d", n),
			CampaignID: fmt.Sprintf("li-%d", n), PlacementID: "pl-sim-video",
			PublisherID: "pub", ClearingPrice: 5, Currency: "USD", MediaURL: "https://cdn/ad.mp4",
		})
	}))
	defer ssp.Close()

	transcoder := fakeTranscoder(6, 6, 3) // each ad conditions to 15s
	defer transcoder.Close()

	d := &stitcherDeps{
		sspURL: ssp.URL, trackerURL: tracker.URL, publicURL: "http://pub.local",
		placementFn:   func() string { return "pl-sim-video" },
		transcoderURL: transcoder.URL,
		maxPodAdsFn:   func() int { return 4 },
		client:        &http.Client{},
	}

	rec := httptest.NewRecorder()
	d.manifestHandler(rec, httptest.NewRequest("GET", "/v1/ssai/manifest.m3u8", nil))
	out := rec.Body.String()
	if _, err := ssai.ParseMedia(out); err != nil {
		t.Fatalf("stitched manifest invalid: %v", err)
	}

	replaySegments(d, out)
	mu.Lock()
	defer mu.Unlock()
	// Break 0 (30s avail) takes two 15s ads; break 1 (15s avail) takes one → 3.
	// Without pods this would be 2 (one ad per break).
	if impressions != 3 {
		t.Errorf("want 3 impressions (2-ad pod in the 30s avail + 1 in the 15s), got %d", impressions)
	}
}

// TestSlateFillsUnfilledBreak asserts that when no ad bids, a configured slate
// clip is spliced into the avail (with no beacons) rather than dropping to
// content.
func TestSlateFillsUnfilledBreak(t *testing.T) {
	var mu sync.Mutex
	var impressions int
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if strings.Contains(r.URL.Path, "/v1/t/imp") {
			impressions++
		}
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer tracker.Close()

	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sspWinner{NoBid: true}) // nobody bids
	}))
	defer ssp.Close()

	transcoder := fakeTranscoder(6, 6) // slate conditions to 12s
	defer transcoder.Close()

	d := &stitcherDeps{
		sspURL: ssp.URL, trackerURL: tracker.URL, publicURL: "http://pub.local",
		placementFn:     func() string { return "pl-sim-video" },
		transcoderURL:   transcoder.URL,
		maxPodAdsFn:     func() int { return 4 },
		slateCreativeFn: func() string { return "house-slate" },
		slateMediaFn:    func() string { return "https://cdn/slate.mp4" },
		client:          &http.Client{},
	}

	rec := httptest.NewRecorder()
	d.manifestHandler(rec, httptest.NewRequest("GET", "/v1/ssai/manifest.m3u8", nil))
	out := rec.Body.String()

	if strings.Contains(out, "content_000.ts") || strings.Contains(out, "content_005.ts") {
		t.Errorf("unfilled avail not slated (content-during-break kept):\n%s", out)
	}
	if !strings.Contains(out, "/v1/ssai/seg?") {
		t.Errorf("slate segments not stitched:\n%s", out)
	}
	if _, err := ssai.ParseMedia(out); err != nil {
		t.Errorf("slated manifest invalid: %v", err)
	}

	replaySegments(d, out)
	mu.Lock()
	defer mu.Unlock()
	if impressions != 0 {
		t.Errorf("slate must not fire impressions, got %d", impressions)
	}
}

// TestAudioStitch asserts the audio SSAI path: ?channel=audio runs an audio
// auction, conditions to the audio-only profile, stitches into the built-in
// audio origin, and routes quartile beacons through /v1/t/audio (not video).
func TestAudioStitch(t *testing.T) {
	var mu sync.Mutex
	var audioEvents, videoEvents, impressions int
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		switch {
		case strings.Contains(r.URL.Path, "/v1/t/audio"):
			audioEvents++
		case strings.Contains(r.URL.Path, "/v1/t/video"):
			videoEvents++
		case strings.Contains(r.URL.Path, "/v1/t/imp"):
			impressions++
		}
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer tracker.Close()

	var mu2 sync.Mutex
	var sspChannel string
	var call int
	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu2.Lock()
		sspChannel = r.URL.Query().Get("channel")
		call++
		n := call
		mu2.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sspWinner{
			TraceID: fmt.Sprintf("t-%d", n), CreativeID: fmt.Sprintf("cr-%d", n),
			CampaignID: fmt.Sprintf("li-%d", n), PlacementID: "pl-audio",
			PublisherID: "pub", ClearingPrice: 3, Currency: "USD", MediaURL: "https://cdn/spot.mp3",
		})
	}))
	defer ssp.Close()

	// Transcoder that asserts it was asked for an audio-only profile.
	var gotAudioProfile bool
	transcoder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cache_only") != "1" {
			w.WriteHeader(200)
			return
		}
		var req struct {
			Profile transcode.Profile `json:"profile"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Profile.AudioOnly {
			mu2.Lock()
			gotAudioProfile = true
			mu2.Unlock()
		}
		var out transcode.Conditioned
		out.Cached = true
		for i, ds := range []float64{6, 6, 3} { // 15s audio ad
			out.Segments = append(out.Segments, transcode.CondSegment{
				URI: fmt.Sprintf("http://cdn.local/cond/aud_%d.ts", i), Duration: ds,
			})
			out.Duration += ds
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer transcoder.Close()

	d := &stitcherDeps{
		sspURL: ssp.URL, trackerURL: tracker.URL, publicURL: "http://pub.local",
		placementFn:   func() string { return "pl-audio" },
		transcoderURL: transcoder.URL,
		maxPodAdsFn:   func() int { return 4 },
		client:        &http.Client{},
	}

	rec := httptest.NewRecorder()
	d.manifestHandler(rec, httptest.NewRequest("GET", "/v1/ssai/manifest.m3u8?channel=audio", nil))
	out := rec.Body.String()

	// The built-in audio origin's break content (audio_002) must be replaced.
	if strings.Contains(out, "audio_002.ts") {
		t.Errorf("audio break content not replaced:\n%s", out)
	}
	if !strings.Contains(out, "/v1/ssai/seg?") {
		t.Errorf("no ad stitched into the audio avail:\n%s", out)
	}
	if _, err := ssai.ParseMedia(out); err != nil {
		t.Errorf("stitched audio manifest invalid: %v", err)
	}

	replaySegments(d, out)

	mu2.Lock()
	if sspChannel != "audio" {
		t.Errorf("SSP called with channel=%q, want audio", sspChannel)
	}
	if !gotAudioProfile {
		t.Error("transcoder was not asked for an audio-only profile")
	}
	mu2.Unlock()

	mu.Lock()
	defer mu.Unlock()
	if audioEvents == 0 {
		t.Error("no quartile beacons routed through /v1/t/audio")
	}
	if videoEvents != 0 {
		t.Errorf("audio stitch fired %d /v1/t/video beacons, want 0", videoEvents)
	}
	// 30s avail → two 15s ads → two impressions.
	if impressions != 2 {
		t.Errorf("want 2 audio impressions (2-ad pod), got %d", impressions)
	}
}

// TestServeMasterRewritesRenditions asserts a demuxed ABR master (separate audio
// group) has BOTH its video variants and its #EXT-X-MEDIA audio rendition
// rewritten to stitcher URLs — the audio via channel=audio so its ad is
// conditioned to match — while the variants keep their AUDIO group reference.
func TestServeMasterRewritesRenditions(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-VERSION:4\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"English\",DEFAULT=YES,URI=\"audio/en/index.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=928000,RESOLUTION=640x360,CODECS=\"avc1.4d401e\",AUDIO=\"aud\"\n" +
		"360p/index.m3u8\n"

	d := &stitcherDeps{publicURL: "http://pub.local"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/ssai/manifest.m3u8?geo=USA", nil)
	d.serveMaster(rec, req, master, "http://origin.local/hls/master.m3u8", log)
	out := rec.Body.String()

	// Audio rendition URI → a stitcher URL carrying channel=audio + absolute origin.
	if !strings.Contains(out, "/v1/ssai/manifest.m3u8") || !strings.Contains(out, "channel=audio") {
		t.Errorf("audio rendition not rewritten to an audio stitcher URL:\n%s", out)
	}
	if !strings.Contains(out, url.QueryEscape("http://origin.local/hls/audio/en/index.m3u8")) {
		t.Errorf("audio rendition origin not made absolute:\n%s", out)
	}
	// Variant keeps its AUDIO group ref, and its URI is a stitcher URL with rung.
	if !strings.Contains(out, `AUDIO="aud"`) {
		t.Errorf("variant lost AUDIO group reference:\n%s", out)
	}
	if !strings.Contains(out, "rung=360") {
		t.Errorf("video variant not rewritten with its rung:\n%s", out)
	}
}

// TestDASHManifest asserts the DASH path: ?format=mpd runs the same stitch
// pipeline but renders a multi-period MPD — content periods + ad periods, the ad
// period referencing the conditioned CMAF init + segments.
func TestDASHManifest(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer tracker.Close()

	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sspWinner{
			TraceID: "t-dash", CreativeID: "cr-v", CampaignID: "li-v", PlacementID: "pl",
			PublisherID: "pub", ClearingPrice: 5, Currency: "USD", MediaURL: "https://cdn/ad.mp4",
		})
	}))
	defer ssp.Close()

	// CMAF-conditioned ad: fMP4 init + .m4s segments.
	transcoder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cache_only") != "1" {
			w.WriteHeader(200)
			return
		}
		out := transcode.Conditioned{Cached: true, InitURI: "http://cdn.local/cond/init.mp4"}
		for i, ds := range []float64{6, 6} {
			out.Segments = append(out.Segments, transcode.CondSegment{URI: fmt.Sprintf("http://cdn.local/cond/seg_%d.m4s", i), Duration: ds})
			out.Duration += ds
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer transcoder.Close()

	d := &stitcherDeps{
		sspURL: ssp.URL, trackerURL: tracker.URL, publicURL: "http://pub.local",
		placementFn: func() string { return "pl" }, transcoderURL: transcoder.URL,
		maxPodAdsFn: func() int { return 1 }, client: &http.Client{},
	}

	rec := httptest.NewRecorder()
	d.manifestHandler(rec, httptest.NewRequest("GET", "/v1/ssai/manifest.mpd", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/dash+xml" {
		t.Fatalf("content-type = %q, want application/dash+xml", ct)
	}
	out := rec.Body.String()
	if !dash.IsMPD(out) {
		t.Fatalf("response is not an MPD:\n%s", out)
	}
	mpd, err := dash.ParseMPD(out)
	if err != nil {
		t.Fatalf("MPD parse: %v", err)
	}
	// Sample content has 2 breaks → content/ad periods interleaved; at least one
	// ad period must exist, carrying the conditioned init + a /v1/ssai/seg URL.
	var adPeriods int
	for _, p := range mpd.Periods {
		if !strings.HasPrefix(p.ID, "ad-") {
			continue
		}
		adPeriods++
		sl := p.AdaptationSets[0].Representations[0].SegmentList
		if sl.Initialization == nil || !strings.Contains(sl.Initialization.SourceURL, "init.mp4") {
			t.Errorf("ad period missing conditioned init: %+v", sl.Initialization)
		}
		if len(sl.SegmentURLs) == 0 || !strings.Contains(sl.SegmentURLs[0].Media, "/v1/ssai/seg") {
			t.Errorf("ad period segment not a stitcher beacon URL: %+v", sl.SegmentURLs)
		}
	}
	if adPeriods == 0 {
		t.Errorf("no ad periods in the MPD:\n%s", out)
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
