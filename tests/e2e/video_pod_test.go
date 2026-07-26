//go:build e2e

// CTV ad pods: GET /v1/pubad/video/vast?pod=N runs up to N·3 independent
// auctions and assembles a single sequenced-ad VAST pod with competitive
// separation — no advertiser may appear twice in the pod. That separation logic
// (buildPodVAST in cmd/publisher-adserver) had no e2e. The "no repeated
// advertiser" invariant holds for any fill count, so this is deterministic even
// though how many ads fill isn't.
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

func TestVideoPodCompetitiveSeparation(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.RefreshAllCaches(t)

	url := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=pl-sport-live-preroll&pod=3"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("pod vast request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pod vast status = %d, body=%s", resp.StatusCode, body)
	}

	var doc struct {
		XMLName xml.Name `xml:"VAST"`
		Version string   `xml:"version,attr"`
		Ads     []struct {
			Sequence string `xml:"sequence,attr"`
			InLine   struct {
				Advertiser string `xml:"Advertiser"`
			} `xml:"InLine"`
		} `xml:"Ad"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("pod response is not valid VAST XML: %v\n%s", err, body)
	}
	if doc.Version != "4.2" {
		t.Errorf("VAST version = %q, want 4.2", doc.Version)
	}

	// Core invariant: an advertiser must never repeat within the pod (checked on
	// non-empty domains — that's the separation key buildPodVAST dedups on).
	seen := map[string]bool{}
	for _, ad := range doc.Ads {
		adv := strings.ToLower(strings.TrimSpace(ad.InLine.Advertiser))
		if adv == "" {
			continue
		}
		if seen[adv] {
			t.Errorf("advertiser %q appears more than once in the pod — competitive separation broken", adv)
		}
		seen[adv] = true
	}

	// When more than one ad fills, the pod must be ordered (each Ad carries a
	// sequence attribute) so the player plays them back-to-back in order.
	if len(doc.Ads) > 1 {
		for i, ad := range doc.Ads {
			if ad.Sequence == "" {
				t.Errorf("pod Ad #%d has no sequence attribute (pod ordering required)", i)
			}
		}
	}
	t.Logf("pod requested 3, filled %d distinct-advertiser ads", len(doc.Ads))
}
