//go:build e2e

// SSAI server-side beaconing: the stitcher fills an ad break with a real video
// auction, conditions the winning creative into HLS segments, and points each ad
// segment at /v1/ssai/seg — which fires that segment's quartile beacon
// SERVER-SIDE when the player fetches it, then 302-redirects to the media. The
// existing SSAI test is a manifest smoke test; this proves the beacon actually
// reaches the tracker → reporting. Builds real video inventory (a conditionable
// creative) so the break fills and stitches.
package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSSAIServerSideBeaconReachesReporting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("ssai-%d", time.Now().UnixNano())
	pubAcc := h.CreatePublisher(t, "ss-pub-"+uniq)
	pub := h.AddPublisher(t, pubAcc, "ss-pub-"+uniq, "ss-"+uniq+".test")
	h.AddVideoPlacement(t, pub, "ss-pl-"+uniq, 1.00, 1, 60)

	adv := h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, adv.ID, 100_000, "ss-grant-"+uniq)
	vio := h.CreateInsertionOrder(t, adv, "ss-io-"+uniq, 5000)
	camp := h.CreateVideoCampaign(t, adv, vio, "ss-li-"+uniq, 10.0, 500,
		"ss-cr-"+uniq, "acme-"+uniq+".test", 6, harness.Targeting{})

	// Point the creative at a REAL conditionable asset (the seed's sintel sample,
	// served from the creatives store) so the SSAI transcoder can slice it into
	// HLS segments — a fabricated .mp4 URL would fail conditioning and never
	// stitch. The conditioner rewrites our own creatives/media URLs to the
	// object-store key, so this is fetched from Minio, not over the network.
	if _, err := h.DB.Exec(`UPDATE creatives SET asset_url = $1 WHERE id = $2::uuid`,
		"http://localhost:8080/v1/creatives/media/sintel-360-1mb.mp4", camp.CreativeID); err != nil {
		t.Fatalf("point creative at sintel: %v", err)
	}
	h.RefreshAllCaches(t)

	// The stitcher polls the manifest rapidly from ONE IP (→ one household) for
	// ONE campaign to catch the first-time transcode. The video serve path is now
	// household-freq-capped (adserver.freq_cap_per_user_per_campaign, default 5) —
	// SSP returns nobid on a 429 — so within a few polls the cap trips and every
	// later video auction nobids → the break never fills within the window. A real
	// CTV viewer requests the manifest once; raise the cap so the poll can fill.
	const fcPod, fcKey = "adserver-0", "adserver.freq_cap_per_user_per_campaign"
	h.SetConfigForPod(t, fcKey, "100000", fcPod)
	t.Cleanup(func() { h.SetConfigForPod(t, fcKey, "5", fcPod) })

	// Poll the manifest until the break fills AND the ad is conditioned into
	// segments (first-time transcode is slow) — an ad segment is a /v1/ssai/seg
	// URL. Extract its `ad` param (the pod ad's trace) to correlate beacons.
	noRedirect := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     harness.RetryTransport(), // survive port-forward flaps under load
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var segURL, adTrace string
	harness.WaitFor(t, 90*time.Second, "SSAI stitches an ad segment", func() bool {
		req, _ := http.NewRequest(http.MethodGet,
			h.URLs.SSAI+routes.SSAIManifest+"?placement_id=ss-pl-"+uniq+"&device=ctv&channel=video", nil)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.Contains(line, routes.SSAISegment) {
				continue
			}
			u, err := url.Parse(line)
			if err != nil {
				continue
			}
			if ad := u.Query().Get("ad"); ad != "" {
				segURL, adTrace = line, ad
				return true
			}
		}
		return false
	})
	t.Logf("stitched ad segment trace=%s", adTrace)

	// Fetch the ad segment as a player would — the SSAI handler fires the
	// server-side quartile beacon(s), then 302-redirects to the conditioned media.
	req, _ := http.NewRequest(http.MethodGet, segURL, nil)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("fetch ad segment: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		t.Errorf("ad segment fetch = %d, want a 3xx redirect to the media", resp.StatusCode)
	}

	// The server-side beacon must reach reporting as a video media event on this
	// ad's trace (tracker → NATS → reporting is async).
	harness.WaitFor(t, 20*time.Second, "SSAI quartile beacon lands in reporting", func() bool {
		return h.MediaEventsByTrace(t, adTrace, "video", "") >= 1
	})
}
