//go:build e2e

// The ad server → tracker ARA source loop: with adserver.ara_source_registration
// on, a CONSENTED serve carries a signed ara_source_url (attributionsrc beacon),
// and firing that beacon actually registers a source at the tracker (returns the
// Attribution-Reporting-Register-Source header). An unconsented serve carries no
// beacon. This is the missing integration link that lets a real browser register
// a source — proven end to end without a browser only up to the registration
// (the match/noise/delay remain the documented mock boundary).
package e2e

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestARAAdServerBakesSourceBeacon(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "araad")
	h.RefreshAllCaches(t)

	t.Cleanup(func() {
		h.SetConfigForPod(t, "adserver.ara_source_registration", "false", "adserver-0")
		h.SetConfigForPod(t, "tracker.ara_enabled", "false", "tracker-0")
	})
	h.SetConfigForPod(t, "tracker.ara_enabled", "true", "tracker-0")
	h.SetConfigForPod(t, "adserver.ara_source_registration", "true", "adserver-0")

	base := models.ServeRequest{
		CampaignID: w.Campaign.ID, CreativeID: w.Campaign.CreativeID,
		PlacementID: w.Placement.ID, PublisherID: w.Publisher.ID, AdvertiserID: w.AdvAcc.ID,
		ClearingPrice: 1.00, Currency: "USD", SiteDomain: w.Publisher.Domain, Width: 300, Height: 250,
	}

	// Consented serve (BehaviourUserID present == personalisation consent) → beacon baked.
	consented := base
	consented.TraceID = "araad-consented"
	consented.BehaviourUserID = "ara-consented-user"
	resp := h.ServeAdResponse(t, consented)
	if resp.ARASourceURL == "" {
		t.Fatal("adserver did not bake ara_source_url on a consented serve with ARA enabled")
	}
	if !strings.Contains(resp.ARASourceURL, "/v1/t/ara/src") {
		t.Errorf("ara_source_url = %q, want a /v1/t/ara/src beacon", resp.ARASourceURL)
	}

	// The baked (signed) beacon actually registers a source at the tracker. Fire
	// the same signed path+params against the reachable tracker host (the
	// signature covers path+params, not host, so it validates).
	u, err := url.Parse(resp.ARASourceURL)
	if err != nil {
		t.Fatalf("parse ara_source_url: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Tracker+u.Path+"?"+u.RawQuery, nil)
	sresp, err := harness.NewHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("fire ara source beacon: %v", err)
	}
	hdr := sresp.Header.Get(ara.HeaderRegisterSource)
	code := sresp.StatusCode
	sresp.Body.Close()
	if hdr == "" {
		t.Errorf("firing the adserver's ARA beacon returned no register-source header (status %d) — adserver→tracker loop broken", code)
	}

	// Unconsented serve (no BehaviourUserID) → no beacon.
	noConsent := base
	noConsent.TraceID = "araad-noconsent"
	if got := h.ServeAdResponse(t, noConsent); got.ARASourceURL != "" {
		t.Errorf("ara_source_url baked on an UNCONSENTED serve: %q", got.ARASourceURL)
	}
}
