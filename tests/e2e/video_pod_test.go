//go:build e2e

// CTV ad pods: GET /v1/pubad/video/vast?pod=N assembles a sequenced-ad VAST pod
// with competitive separation. This builds real video inventory (two funded
// video advertisers) and a pod request, then asserts the pod is a well-formed
// sequenced VAST with NO repeated advertiser.
//
// FINDING (see docs/E2E_COVERAGE_GAPS.md): buildPodVAST only SKIPS a repeated
// advertiser post-hoc — it re-runs each sub-auction with identical params and
// never EXCLUDES already-picked advertisers. So when one advertiser wins
// deterministically (equal bids, no auction variance), a pod of 3 fills only 1
// ad. Real competitive separation should exclude picked seats from later
// sub-auctions. This test therefore asserts the invariant that always holds (no
// repeat + valid sequenced VAST + real fill) and logs how many distinct
// advertisers actually filled, rather than assuming diversity the impl can't
// guarantee under concentrated demand.
package e2e

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestVideoPodCompetitiveSeparation(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("vpod-%d", time.Now().UnixNano())
	pubAcc := h.CreatePublisher(t, "vp-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "vp-pub-"+uniq, "vp-"+uniq+".test")
	h.AddVideoPlacement(t, pub, "vp-pl-"+uniq, 1.00, 6, 30)

	for i, name := range []string{"adv-acme", "adv-globex"} {
		adv := h.CreateAdvertiser(t, name)
		h.GrantBalance(t, adv.ID, 100_000, fmt.Sprintf("vp-grant-%d-%s", i, uniq))
		vio := h.CreateInsertionOrder(t, adv, fmt.Sprintf("vp-io-%d-%s", i, uniq), 5000)
		h.CreateVideoCampaign(t, adv, vio, fmt.Sprintf("vp-li-%d-%s", i, uniq), 10.0, 500,
			fmt.Sprintf("vp-cr-%d-%s", i, uniq), fmt.Sprintf("brand%d-%s.test", i, uniq), 15, harness.Targeting{})
	}
	h.RefreshAllCaches(t)

	type podDoc struct {
		XMLName xml.Name `xml:"VAST"`
		Version string   `xml:"version,attr"`
		Ads     []struct {
			Sequence string `xml:"sequence,attr"`
			InLine   struct {
				Advertiser string `xml:"Advertiser"`
			} `xml:"InLine"`
		} `xml:"Ad"`
	}

	// Poll until the pod fills at least one real video ad (we seeded inventory).
	var doc podDoc
	harness.WaitFor(t, 25*time.Second, "pod fills ≥1 real video ad", func() bool {
		url := h.URLs.PublisherAdServer + routes.PublisherAdServeVAST + "?placement_id=vp-pl-" + uniq + "&pod=3"
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("pod request: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		doc = podDoc{}
		if xml.Unmarshal(b, &doc) != nil {
			return false
		}
		return len(doc.Ads) >= 1
	})

	if doc.Version != "4.2" {
		t.Errorf("VAST version = %q, want 4.2", doc.Version)
	}

	// The invariant that MUST hold regardless of fill count: no advertiser
	// repeats within the pod.
	seen := map[string]bool{}
	for _, ad := range doc.Ads {
		adv := ad.InLine.Advertiser
		if adv == "" {
			continue
		}
		if seen[adv] {
			t.Errorf("advertiser %q appears more than once in the pod — competitive separation broken", adv)
		}
		seen[adv] = true
	}

	// Pod ordering: each Ad carries a sequence attribute.
	for i, ad := range doc.Ads {
		if ad.Sequence == "" {
			t.Errorf("pod Ad #%d has no sequence attribute (pod ordering required)", i)
		}
	}
	t.Logf("pod filled %d ad(s) across %d distinct advertiser(s)", len(doc.Ads), len(seen))
}
