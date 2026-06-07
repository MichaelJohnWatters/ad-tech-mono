package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vmap"
)

// vmapHandler must return a VMAP document with exactly three breaks
// (pre / mid / post), each pointing at the VAST endpoint with a break
// identifier so the upstream auctions can be correlated to the slot.
func TestVMAPHandler(t *testing.T) {
	h := vmapHandler(nullLogger(), "http://localhost:8080")

	req := httptest.NewRequest("GET", "/v1/pubad/video/vmap?placement_id=demo-video", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/xml") {
		t.Errorf("Content-Type = %q, want application/xml", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	// XML emitted with the vmap: namespace prefix doesn't round-trip
	// through encoding/xml unmarshal cleanly (the colon trips the
	// decoder), so we assert on the rendered string instead — same
	// pattern pkg/vmap/build_test uses. The shape is small enough
	// that string contains is more honest than half-decoded structs.
	s := rec.Body.String()
	if !strings.Contains(s, `<vmap:VMAP xmlns:vmap="http://www.iab.net/videosuite/vmap" version="1.0">`) {
		t.Errorf("VMAP root element missing or wrong:\n%s", s)
	}
	for _, want := range []string{
		// Three breaks at the right offsets with the right break IDs.
		`<vmap:AdBreak breakType="linear" timeOffset="start" breakId="pre-roll">`,
		`<vmap:AdBreak breakType="linear" timeOffset="00:00:30" breakId="mid-roll-1">`,
		`<vmap:AdBreak breakType="linear" timeOffset="end" breakId="post-roll">`,
		// Each break calls back into /v1/pubad/video/vast with its break fragment.
		`<vmap:AdTagURI templateType="vast4.2">`,
		`/v1/pubad/video/vast?placement_id=demo-video&break=pre`,
		`/v1/pubad/video/vast?placement_id=demo-video&break=mid`,
		`/v1/pubad/video/vast?placement_id=demo-video&break=post`,
		// Per-break tracking events present.
		`<vmap:Tracking event="breakStart">`,
		`<vmap:Tracking event="breakEnd">`,
		`br=pre`,
		`br=mid`,
		`br=post`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in VMAP output:\n%s", want, s)
		}
	}
	// And cross-check vmap.Version stays the constant the builder uses.
	if !strings.Contains(s, `version="`+vmap.Version+`"`) {
		t.Errorf("VMAP version attribute did not match vmap.Version constant: %s", s)
	}
}

// Placement query param is forwarded into the per-break AdTagURI so the
// downstream VAST auctions see it too.
func TestVMAPHandler_ForwardsPlacementID(t *testing.T) {
	h := vmapHandler(nullLogger(), "http://localhost:8080")

	req := httptest.NewRequest("GET", "/v1/pubad/video/vmap?placement_id=custom-placement-xyz", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "placement_id=custom-placement-xyz") {
		t.Errorf("AdTagURI did not forward placement_id=custom-placement-xyz: %s", body)
	}
}

// Empty placement query → defaults so the demo always produces a
// valid schedule even if the caller forgot to set it.
func TestVMAPHandler_DefaultPlacement(t *testing.T) {
	h := vmapHandler(nullLogger(), "http://localhost:8080")

	req := httptest.NewRequest("GET", "/v1/pubad/video/vmap", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "placement_id=pl-sport-mpu") {
		t.Errorf("default placement not used: %s", body)
	}
}
