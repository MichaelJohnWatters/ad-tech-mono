package main

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// stubAudioSSP returns an audio winner mimicking the real SSP's channel=audio
// response, so the audio handler can be exercised without a real SSP. It also
// asserts the handler forwards the visitor's privacy/identity signals (Phase 2)
// rather than collapsing to a hardcoded request.
func stubAudioSSP(t *testing.T, winner sspVideoWinner) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("channel") != "audio" {
			t.Errorf("stub SSP got channel=%q, want audio", q.Get("channel"))
		}
		if q.Get("gdpr") != "1" || q.Get("geo") != "DEU" {
			t.Errorf("audio handler dropped visitor signals: gdpr=%q geo=%q", q.Get("gdpr"), q.Get("geo"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, winner)
	}))
}

func TestAudioHandler(t *testing.T) {
	ssp := stubAudioSSP(t, sspVideoWinner{
		TraceID:          "trace-audio",
		Channel:          "audio",
		CreativeID:       "cr-shoes-audio-30s",
		CampaignID:       "li-audio-001",
		PlacementID:      "pl-sim-audio",
		PublisherID:      "pub-sim",
		AdvertiserID:     "adv-acme",
		AdvertiserDomain: "acme-shoes.com",
		BidModel:         "cpm",
		Currency:         "USD",
		ClearingPrice:    4.20,
		DurationSeconds:  30,
		MediaURL:         "https://cdn.example/acme-audio-30s.mp3",
	})
	defer ssp.Close()

	h := audioHandler(nullLogger(), "http://tracker:8083", ssp.URL, "https://gateway.adtech.local", alwaysStub, noHouseAds)

	// Visitor is an EU GDPR-consented listener — signals must reach the SSP.
	req := httptest.NewRequest("GET", "/v1/pubad/audio?placement_id=pl-sim-audio&geo=DEU&gdpr=1&consent=abc&device=mobile", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("Content-Type = %q, want xml", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal audio VAST: %v\nbody=%s", err, rec.Body.String())
	}
	if len(doc.Ads) != 1 || doc.Ads[0].InLine == nil {
		t.Fatalf("want 1 InLine Ad, got %+v", doc.Ads)
	}
	cr := doc.Ads[0].InLine.Creatives.Creatives[0]
	if cr.Linear == nil || len(cr.Linear.MediaFiles.MediaFiles) == 0 {
		t.Fatal("Linear/MediaFiles missing")
	}
	mf := cr.Linear.MediaFiles.MediaFiles[0]
	if !strings.HasPrefix(mf.Type, "audio/") {
		t.Errorf("MediaFile type = %q, want audio/*", mf.Type)
	}
	if mf.Width != 0 || mf.Height != 0 {
		t.Errorf("audio MediaFile must not carry width/height, got %dx%d", mf.Width, mf.Height)
	}

	// Beacons must route through /v1/t/audio so the tracker publishes typed
	// AudioEvent, not video/display events.
	body := rec.Body.String()
	if !strings.Contains(body, "/v1/t/audio?") {
		t.Errorf("quartile beacons must route through /v1/t/audio; body=%s", body)
	}
	if strings.Contains(body, "/v1/t/video?") {
		t.Errorf("audio VAST must not emit /v1/t/video beacons")
	}
}

// TestAudioHandlerNoBidHouseAd asserts that on a no-bid with the fallback on AND
// an audio house ad configured, the handler serves the house ad's own markup
// (valid VAST) — not a hardcoded canned stub.
func TestAudioHandlerNoBidHouseAd(t *testing.T) {
	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, sspVideoWinner{NoBid: true})
	}))
	defer ssp.Close()

	houseFn := houseAdFrom(houseads.HouseAd{
		ID: "11111111-1111-4111-8111-111111111111", Format: houseads.FormatAudio,
		Name: "House Audio", Markup: houseAudioVAST, Enabled: true, Weight: 1,
	})
	h := audioHandler(nullLogger(), "http://tracker:8083", ssp.URL, "https://gateway.adtech.local", alwaysStub, houseFn)
	req := httptest.NewRequest("GET", "/v1/pubad/audio?placement_id=pl-sim-audio", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("no-bid status = %d, want 200 (house ad fallback)", rec.Code)
	}
	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("house-ad audio markup is not valid VAST: %v", err)
	}
	if len(doc.Ads) != 1 || doc.Ads[0].ID != "house-audio" {
		t.Errorf("expected the configured house ad's VAST (Ad id house-audio), got %+v", doc.Ads)
	}
}

// TestAudioHandlerNoBidNoHouseAd: fallback on but NO audio house ad configured →
// honest empty VAST (no fake/canned content invented).
func TestAudioHandlerNoBidNoHouseAd(t *testing.T) {
	ssp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, sspVideoWinner{NoBid: true})
	}))
	defer ssp.Close()

	h := audioHandler(nullLogger(), "http://tracker:8083", ssp.URL, "https://gateway.adtech.local", alwaysStub, noHouseAds)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/v1/pubad/audio?placement_id=pl-sim-audio", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (empty VAST)", rec.Code)
	}
	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("empty VAST must still parse: %v", err)
	}
	if len(doc.Ads) != 0 {
		t.Errorf("no configured audio house ad → 0 Ads (honest no-fill), got %d", len(doc.Ads))
	}
}
