//go:build e2e

// The video VAST endpoint (GET /v1/pubad/video/vast) runs a real auction
// (SSP→exchange→DSP) and renders VAST 4.2 — but only pkg/vast's unit tests
// covered the XML; nothing exercised the live endpoint. This asserts the
// response is always well-formed VAST 4.2, that a filled ad carries a tracked
// impression + media file + quartile beacons routed through /v1/t/video, and —
// critically — that a no-fill returns an honest Ad-less document with NO
// fabricated <Impression> (the real-data-only rule).
package e2e

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestVideoVASTResponse(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.RefreshAllCaches(t) // ensure the warm caches hold the seeded video inventory

	// pl-sport-live-preroll is a seeded VIDEO placement
	// (profiles/publishers/standard.yaml).
	url := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=pl-sport-live-preroll"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("vast request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vast status = %d, body=%s", resp.StatusCode, body)
	}

	// Always: a well-formed VAST 4.2 document.
	var doc struct {
		XMLName xml.Name   `xml:"VAST"`
		Version string     `xml:"version,attr"`
		Ads     []struct{} `xml:"Ad"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("response is not valid VAST XML: %v\n%s", err, body)
	}
	if doc.XMLName.Local != "VAST" || doc.Version != "4.2" {
		t.Errorf("root = <%s version=%q>, want <VAST version=\"4.2\">", doc.XMLName.Local, doc.Version)
	}

	s := string(body)
	hasImpression := strings.Contains(s, "<Impression")
	if len(doc.Ads) > 0 {
		// Filled: must carry a tracked impression, a media file, and quartile
		// beacons routed through the video tracker path.
		if !hasImpression {
			t.Error("filled VAST has an <Ad> but no <Impression> tracker")
		}
		if !strings.Contains(s, "<MediaFile") {
			t.Error("filled VAST has no <MediaFile>")
		}
		if !strings.Contains(s, routes.TrackerVideo) {
			t.Error("filled VAST quartile beacons don't route through /v1/t/video")
		}
	} else {
		// Honest no-fill: an Ad-less VAST must NOT fabricate an impression.
		if hasImpression {
			t.Error("no-fill VAST contains an <Impression> — must not fabricate an impression (real-data-only rule)")
		}
		t.Log("video placement returned no-fill; asserted honest empty VAST (no impression tag)")
	}
}
