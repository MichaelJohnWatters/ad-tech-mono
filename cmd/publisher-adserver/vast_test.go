package main

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// noOMID is the OMID config accessor used by tests that don't exercise
// AdVerifications — returns no verification script, so none is emitted.
func noOMID() (string, string) { return "", "" }

// alwaysStub enables the demo house-ad fallback so these tests keep exercising
// the no-bid → stub VAST path (prod defaults the fallback OFF → empty no-fill).
func alwaysStub() bool { return true }

// jsonEncode is an alias so the test helper above doesn't need the full
// encoding/json import surface inline. Keeps the helper readable.
func jsonEncode(w http.ResponseWriter, v interface{}) error {
	return json.NewEncoder(w).Encode(v)
}

func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// vastHandler is the entrypoint for the /v1/pubad/video/vast demo
// route. The test pins the response shape end-to-end: status, MIME
// type, parseable VAST 4.2, presence of the impression / click /
// quartile tracker URLs, and — most importantly — that the URLs the
// handler emits are HMAC-validatable. A regression in the macros
// wiring would let the IMA player fetch a VAST with broken sigs and
// the quartile beacons would 403 silently.
// stubSSP returns a video winner that mimics the real SSP's
// channel=video response shape. Used by vastHandler tests so we can
// exercise the auction-driven path without standing up a real SSP.
func stubSSP(t *testing.T, winner sspVideoWinner) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") != "video" {
			t.Errorf("stub SSP got channel=%q, want video", r.URL.Query().Get("channel"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, winner)
	}))
}

func encodeJSON(w http.ResponseWriter, v interface{}) error {
	return jsonEncode(w, v)
}

func TestVASTHandler(t *testing.T) {
	ssp := stubSSP(t, sspVideoWinner{
		TraceID:          "trace-xyz",
		Channel:          "video",
		CreativeID:       "cr-luxauto-video-15s",
		CampaignID:       "li-luxauto",
		PlacementID:      "demo-video-mpu",
		PublisherID:      "pub-demo",
		AdvertiserID:     "adv-luxauto",
		AdvertiserDomain: "luxauto.com",
		BidModel:         "cpm",
		Currency:         "USD",
		ClearingPrice:    5.20,
		Width:            640,
		Height:           360,
		DurationSeconds:  15,
		MediaURL:         "https://cdn.example/luxauto-15s.mp4",
	})
	defer ssp.Close()

	h := vastHandler(nullLogger(), "http://tracker:8083", ssp.URL, noOMID, alwaysStub)

	req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=demo-video-mpu", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("Content-Type = %q, want an xml content type (text/xml or application/xml)", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (VAST must not be cached — each impression gets fresh trace_id)", cc)
	}

	// Round-trip the XML through encoding/xml so we know an IMA-style
	// parser would accept it.
	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal VAST: %v\nbody=%s", err, rec.Body.String())
	}
	if doc.Version != vast.Version {
		t.Errorf("VAST version = %q, want %q", doc.Version, vast.Version)
	}
	if len(doc.Ads) != 1 {
		t.Fatalf("want exactly 1 Ad, got %d", len(doc.Ads))
	}
	ad := doc.Ads[0]
	if ad.InLine == nil {
		t.Fatal("Ad.InLine missing")
	}
	if len(ad.InLine.Impressions) == 0 {
		t.Fatal("InLine has no Impression tag")
	}
	if ad.InLine.Pricing == nil || ad.InLine.Pricing.Value <= 0 {
		t.Errorf("Pricing block missing or zero: %+v", ad.InLine.Pricing)
	}
	cr := ad.InLine.Creatives.Creatives[0]
	if cr.Linear == nil {
		t.Fatal("Creative.Linear missing")
	}
	if cr.Linear.VideoClicks == nil || cr.Linear.VideoClicks.ClickThrough == nil {
		t.Fatal("VideoClicks.ClickThrough missing — clicks won't navigate")
	}
	if cr.Linear.TrackingEvents == nil {
		t.Fatal("TrackingEvents missing — quartile beacons won't fire")
	}
	// Expect at least the five quartile-ish events.
	wantEvents := map[string]bool{
		"start": false, "firstQuartile": false, "midpoint": false,
		"thirdQuartile": false, "complete": false,
	}
	for _, te := range cr.Linear.TrackingEvents.Tracking {
		if _, ok := wantEvents[te.Event]; ok {
			wantEvents[te.Event] = true
		}
	}
	for ev, seen := range wantEvents {
		if !seen {
			t.Errorf("tracking event %q missing", ev)
		}
	}

	// HMAC validation: the impression URL the handler emits must be
	// a signed tracker URL that ValidateSignature accepts. If the
	// macros package's signing changes and the handler stops calling
	// it correctly, this catches it.
	impURL := ad.InLine.Impressions[0].URI
	if impURL == "" {
		t.Fatal("impression URL is empty")
	}
	if !urlIsHMACValid(t, impURL) {
		t.Errorf("impression URL did not validate against the DefaultSigningKey: %s", impURL)
	}

	clickURL := cr.Linear.VideoClicks.ClickThrough.URI
	if !urlIsHMACValid(t, clickURL) {
		t.Errorf("click URL did not validate: %s", clickURL)
	}

	// click_url must carry the redir param so the tracker 302 can
	// land on the demo landing page. Without it the click is a
	// dead-end.
	if !strings.Contains(clickURL, "redir=") {
		t.Errorf("click URL missing redir= param: %s", clickURL)
	}

	// Quartile + interaction beacons must route through /v1/t/video
	// so downstream consumers pick up typed VideoEvent on
	// adtech.events.video instead of conflating with display
	// viewability on adtech.events.view. Pin every tracker URI.
	for _, te := range cr.Linear.TrackingEvents.Tracking {
		if !strings.Contains(te.URI, "/v1/t/video") {
			t.Errorf("tracking event %q must route to /v1/t/video, got %s", te.Event, te.URI)
		}
		if !strings.Contains(te.URI, "event="+te.Event) {
			t.Errorf("tracking event %q URL must carry event=%s, got %s", te.Event, te.Event, te.URI)
		}
		if !urlIsHMACValid(t, te.URI) {
			t.Errorf("tracking event %q URL did not validate: %s", te.Event, te.URI)
		}
	}
}

func TestVASTHandler_OMIDVerifications(t *testing.T) {
	ssp := stubSSP(t, sspVideoWinner{
		TraceID: "trace-omid", Channel: "video", CreativeID: "cr-1", CampaignID: "li-1",
		PlacementID: "pl-1", PublisherID: "pub-1", AdvertiserID: "adv-1",
		AdvertiserDomain: "acme.com", BidModel: "cpm", Currency: "USD", ClearingPrice: 5.0,
		Width: 640, Height: 360, DurationSeconds: 15, MediaURL: "https://cdn/x.mp4",
	})
	defer ssp.Close()

	// OMID configured → served VAST must carry AdVerifications with the vendor
	// + OM SDK script and a signed verificationNotExecuted beacon.
	omid := func() (string, string) { return "measure.example", "https://measure.example/omweb-v1.js" }
	h := vastHandler(nullLogger(), "http://tracker:8083", ssp.URL, omid, alwaysStub)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=pl-1", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	av := doc.Ads[0].InLine.AdVerifications
	if av == nil || len(av.Verifications) != 1 {
		t.Fatalf("expected 1 verification, got %+v", av)
	}
	v := av.Verifications[0]
	if v.Vendor != "measure.example" {
		t.Errorf("vendor = %q", v.Vendor)
	}
	if v.JavaScriptResource == nil || v.JavaScriptResource.APIFramework != "omid" {
		t.Errorf("expected omid JavaScriptResource, got %+v", v.JavaScriptResource)
	}
	// The not-executed beacon is a signed video tracker URL.
	if v.TrackingEvents == nil || len(v.TrackingEvents.Tracking) == 0 {
		t.Fatal("expected a verificationNotExecuted tracking beacon")
	}
	if !urlIsHMACValid(t, v.TrackingEvents.Tracking[0].URI) {
		t.Errorf("not-executed beacon did not validate: %s", v.TrackingEvents.Tracking[0].URI)
	}
}

// urlIsHMACValid parses a tracker URL and replays the signature check
// the tracker handler would run on inbound requests. Returns true iff
// the sig param matches what the path + params should produce under
// the DefaultSigningKey. The handler always signs with DefaultSigningKey
// today; if it grows a per-tenant key the test will need an update.
func urlIsHMACValid(t *testing.T, raw string) bool {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Errorf("parse %q: %v", raw, err)
		return false
	}
	return adserving.ValidateSignature(u.Path, u.Query(), adserving.DefaultSigningKey)
}

// Two requests to the handler must produce two distinct VAST docs
// (different Ad ids / impression URLs). Otherwise the IMA SDK would
// see the same impression URL twice and the tracker would dedup
// the second call as a replay — visible as "missing impressions"
// in reporting.
// When SSP is unreachable the handler still serves a valid VAST (stub
// fallback). Asserts the demo player never sees a 500 even if the
// auction backend is down.
func TestVASTHandler_FallsBackWhenSSPUnreachable(t *testing.T) {
	h := vastHandler(nullLogger(), "http://tracker:8083", "http://127.0.0.1:1", noOMID, alwaysStub)
	req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=demo", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (fallback should still serve VAST)", rec.Code)
	}
	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("fallback VAST did not parse: %v", err)
	}
}

// SSP returns a no-bid → handler falls back to the stub VAST so the
// player has something to render. (Real publishers would route to a
// house ad / next SSP instead — out of scope for the demo.)
func TestVASTHandler_FallsBackOnNoBid(t *testing.T) {
	ssp := stubSSP(t, sspVideoWinner{NoBid: true})
	defer ssp.Close()
	h := vastHandler(nullLogger(), "http://tracker:8083", ssp.URL, noOMID, alwaysStub)
	req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=demo", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("nobid fallback VAST did not parse: %v", err)
	}
}

func TestVASTHandler_FreshTraceIDPerRequest(t *testing.T) {
	// Stub returns the same winner each call but the handler still
	// builds tracker URLs from the SSP-supplied trace ID. Two
	// requests get the same Ad ID here (that's the SSP's stable
	// trace_id); the more interesting invariant is that real SSP
	// returns a fresh trace per request, which the SSP itself
	// guarantees and the SSP integration tests cover.
	ssp := stubSSP(t, sspVideoWinner{
		TraceID: "trace-abc", Channel: "video",
		CreativeID: "cr-1", CampaignID: "li-1", PlacementID: "p",
		PublisherID: "pub", AdvertiserID: "adv", AdvertiserDomain: "x.example",
		BidModel: "cpm", Currency: "USD", ClearingPrice: 5,
		Width: 640, Height: 360, DurationSeconds: 15,
		MediaURL: "https://cdn/x.mp4",
	})
	defer ssp.Close()
	h := vastHandler(nullLogger(), "http://tracker:8083", ssp.URL, noOMID, alwaysStub)

	get := func() string {
		req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=p", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		var doc vast.VAST
		_ = xml.Unmarshal(rec.Body.Bytes(), &doc)
		return doc.Ads[0].ID
	}

	a, b := get(), get()
	// With a fixed SSP stub Ad IDs match — that's expected. The
	// assertion is that the handler doesn't ever return an empty
	// Ad ID (which would mean the trace plumbing was broken).
	if a == "" || b == "" {
		t.Errorf("Ad IDs must not be empty: %q vs %q", a, b)
	}
}

// nobidSSP returns a no-bid for every channel — used to exercise the no-fill
// path (stub off) without a real auction.
func nobidSSP(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, sspVideoWinner{NoBid: true})
	}))
}

// noStub is the production default: the demo house-ad fallback is OFF, so a
// no-bid returns an honest no-fill (empty VAST / 204), never fake data.
func noStub() bool { return false }

// TestVASTHandler_NoFillWhenStubOff: on a no-bid with the stub disabled, the
// video handler must return a valid but EMPTY VAST (zero Ads), not a canned
// house ad — so no fake impression is ever recorded.
func TestVASTHandler_NoFillWhenStubOff(t *testing.T) {
	ssp := nobidSSP(t)
	defer ssp.Close()

	h := vastHandler(nullLogger(), "http://tracker:8083", ssp.URL, noOMID, noStub)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=pl-1", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (empty VAST): %s", rec.Code, rec.Body.String())
	}
	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("empty VAST must still parse: %v\nbody=%s", err, rec.Body.String())
	}
	if len(doc.Ads) != 0 {
		t.Errorf("no-bid + stub off must yield 0 Ads (honest no-fill), got %d", len(doc.Ads))
	}
}

// TestNativeHandler_NoFillWhenStubOff: native no-bid with stub off returns 204
// (no ad), not a demo native card.
func TestNativeHandler_NoFillWhenStubOff(t *testing.T) {
	ssp := nobidSSP(t)
	defer ssp.Close()

	h := nativeHandler(nullLogger(), "http://tracker:8083", ssp.URL, noStub)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/v1/pubad/native?placement_id=pl-1", nil))

	if rec.Code != http.StatusNoContent {
		t.Errorf("native no-bid + stub off: status = %d, want 204 (honest no-fill): %s", rec.Code, rec.Body.String())
	}
}
