package main

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

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
func TestVASTHandler(t *testing.T) {
	h := vastHandler(nullLogger(), "http://tracker:8083")

	req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=demo-video-mpu", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/xml") {
		t.Errorf("Content-Type = %q, want application/xml", ct)
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
func TestVASTHandler_FreshTraceIDPerRequest(t *testing.T) {
	h := vastHandler(nullLogger(), "http://tracker:8083")

	get := func() string {
		req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=p", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		var doc vast.VAST
		_ = xml.Unmarshal(rec.Body.Bytes(), &doc)
		return doc.Ads[0].ID
	}

	a, b := get(), get()
	if a == b {
		t.Errorf("Ad IDs identical across requests (%q): trace_id must vary per request", a)
	}
}
