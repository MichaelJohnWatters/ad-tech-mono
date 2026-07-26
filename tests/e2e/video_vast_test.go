//go:build e2e

// The video VAST endpoint (GET /v1/pubad/video/vast) runs a real auction
// (SSP→exchange→DSP) and renders VAST 4.2. This builds its OWN video inventory
// (a video placement + a funded video campaign with a video creative) so it
// actually exercises the FILL path — not just the no-fill branch — and asserts
// the winning VAST carries a tracked <Impression>, the creative's <MediaFile>,
// and quartile beacons routed through /v1/t/video. It also asserts the honest
// no-fabricated-impression contract on the no-fill control.
package e2e

import (
	"encoding/xml"
	"fmt"
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
	h.Reset(t)

	uniq := fmt.Sprintf("vvast-%d", time.Now().UnixNano())

	// Publisher + a video placement (6–30s pre-roll window).
	pubAcc := h.CreatePublisher(t, "vv-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "vv-pub-"+uniq, "vv-"+uniq+".test")
	h.AddVideoPlacement(t, pub, "vv-pl-"+uniq, 1.00, 6, 30)

	// adv-acme is allow-listed on the internal DSP; fund it and give it a video
	// campaign with a 15s creative (inside the placement window), match-all
	// targeting, a bid well above the floor.
	adv := h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, adv.ID, 100_000, "vv-grant-"+uniq)
	vio := h.CreateInsertionOrder(t, adv, "vv-io-"+uniq, 5000)
	h.CreateVideoCampaign(t, adv, vio, "vv-li-"+uniq, 10.0, 500,
		"vv-cr-"+uniq, "ford-"+uniq+".test", 15, harness.Targeting{})
	h.RefreshAllCaches(t)

	getVAST := func(t *testing.T) (int, []byte) {
		t.Helper()
		url := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=vv-pl-" + uniq
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("vast request: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, body
	}

	// Poll until the auction fills (cache propagation + first-bid warmup can lag
	// a beat). We built the inventory, so a persistent no-fill is a real failure.
	var body []byte
	harness.WaitFor(t, 20*time.Second, "video auction fills the VAST", func() bool {
		code, b := getVAST(t)
		if code != http.StatusOK {
			return false
		}
		body = b
		return strings.Contains(string(b), "<Ad")
	})

	// Well-formed VAST 4.2.
	var doc struct {
		XMLName xml.Name   `xml:"VAST"`
		Version string     `xml:"version,attr"`
		Ads     []struct{} `xml:"Ad"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("filled response is not valid VAST XML: %v\n%s", err, body)
	}
	if doc.Version != "4.2" {
		t.Errorf("VAST version = %q, want 4.2", doc.Version)
	}
	if len(doc.Ads) == 0 {
		t.Fatalf("no <Ad> in the filled VAST:\n%s", body)
	}

	// The fill path: tracked impression, the creative's media file, quartile
	// beacons through the video tracker.
	s := string(body)
	if !strings.Contains(s, "<Impression") {
		t.Error("filled VAST has no <Impression> tracker")
	}
	if !strings.Contains(s, "<MediaFile") || !strings.Contains(s, ".mp4") {
		t.Error("filled VAST has no <MediaFile> pointing at the video creative")
	}
	if !strings.Contains(s, routes.TrackerVideo) {
		t.Errorf("filled VAST quartile beacons don't route through %s", routes.TrackerVideo)
	}

	// Honest no-fill control: an unknown placement returns an Ad-less doc with
	// NO fabricated <Impression> (real-data-only rule).
	t.Run("unknown_placement_is_honest_nofill", func(t *testing.T) {
		url := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=vv-nope-" + uniq
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("nofill request: %v", err)
		}
		nf, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(nf), "<Impression") {
			t.Errorf("no-fill VAST contains an <Impression> — must not fabricate one:\n%s", nf)
		}
	})
}
