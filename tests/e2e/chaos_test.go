//go:build e2e

// Chaos tests — fail-mode behavior when an infra dep is unhealthy. Each kills
// the relevant pod (via harness.WithChaos → `kubectl -n adtech delete pod`),
// asserts the platform degrades gracefully while it's down, then the harness
// guarantees the pod recovers before the next test runs.
//
// These require kubectl access to the adtech namespace; without it they skip
// (RequireKubectl). They also take longer than a normal e2e case — each waits
// up to ~90s for the killed pod to reschedule and go Ready again.
//
// The assertions are deliberately conservative: "the happy path still works
// while dep X is down" (low false-positive), which is exactly the fail-open /
// fallback contract each dependency has.
package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// Redis down → the DSP budget/balance gate is fail-open (better to risk a small
// overspend than stop bidding), so the campaign must still bid and win.
func TestChaosRedisDownBudgetFailOpen(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	harness.RequireKubectl(t)
	w := harness.BuildBasicWorld(t, h, "chaos-redis")

	h.WithChaos(t, "redis", func() {
		res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "chaos-redis-user")
		win := h.ExtractWinner(t, res)
		if win.NoBid {
			t.Fatal("expected fail-open bid while Redis down, got no_bid")
		}
	})
}

// NATS down → the tracker falls back to POSTing events straight to reporting
// and never fails the pixel response. FireImpression fails the test on a 5xx,
// so its success while NATS is down is the graceful-degradation assertion.
func TestChaosNATSDownTrackerHTTPFallback(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	harness.RequireKubectl(t)
	w := harness.BuildBasicWorld(t, h, "chaos-nats")

	h.WithChaos(t, "nats", func() {
		h.FireImpression(t, "chaos-nats-trace", w.Campaign.ID, w.Campaign.CreativeID,
			w.Placement.ID, w.Publisher.ID, w.AdvAcc.ID, "USD", 3.50)
	})
}

// Postgres down → DSP campaigns and SSP placements are served from warm
// in-memory snapshots, so an auction still resolves against last-known-good
// config instead of erroring. Caches are refreshed before the kill.
func TestChaosPostgresDownCachesServeStaleSnapshots(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	harness.RequireKubectl(t)
	w := harness.BuildBasicWorld(t, h, "chaos-pg")
	h.RefreshAllCaches(t) // ensure warm caches hold the world before Postgres dies

	h.WithChaos(t, "postgres", func() {
		res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "chaos-pg-user")
		win := h.ExtractWinner(t, res)
		if win.NoBid {
			t.Fatal("expected auction served from warm caches while Postgres down, got no_bid")
		}
	})
}

// Minio down → the BuildBasicWorld creative uses the html_content column
// (inline banner) which the ad server serves directly without touching object
// storage, so the serve still returns 200.
func TestChaosMinioDownCreativeServeUsesHTMLContentOnly(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	harness.RequireKubectl(t)
	w := harness.BuildBasicWorld(t, h, "chaos-minio")

	h.WithChaos(t, "minio", func() {
		status := h.ServeAd(t, models.ServeRequest{
			TraceID:       "chaos-minio-trace",
			CampaignID:    w.Campaign.ID,
			CreativeID:    w.Campaign.CreativeID,
			PlacementID:   w.Placement.ID,
			PublisherID:   w.Publisher.ID,
			AdvertiserID:  w.AdvAcc.ID,
			ClearingPrice: 3.50,
			Currency:      "USD",
			SiteDomain:    "chaos-minio.test",
			Width:         300,
			Height:        250,
		})
		if status != http.StatusOK {
			t.Fatalf("html_content creative serve should be 200 while Minio down, got %d", status)
		}
	})
}
