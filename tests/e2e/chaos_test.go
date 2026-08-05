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
	"fmt"
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

// NATS down → the pixel response never fails (the browser already showed the
// ad; failing the beacon can't undo that, only lose the record). The event
// itself lands on the tracker's disk spool for replay — delivery is asserted
// by TestChaosNATSOutageSpoolLossless below; this test only pins the
// "pixel always answers" half. (The old HTTP-fallback-to-reporting path this
// test used to describe was retired in e9256bf: it double-delivered whenever
// a "failed" publish had actually reached the stream.)
func TestChaosNATSDownPixelNeverFails(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	harness.RequireKubectl(t)
	w := harness.BuildBasicWorld(t, h, "chaos-nats")

	h.WithChaos(t, "nats", func() {
		h.FireImpression(t, "chaos-nats-trace", w.Campaign.ID, w.Campaign.CreativeID,
			w.Placement.ID, w.Publisher.ID, w.AdvAcc.ID, "USD", 3.50)
	})
}

// The 2026-08-05 loss mode, pinned as a test: NATS dies mid-traffic, and every
// impression fired during the outage must STILL be recorded EXACTLY ONCE after
// recovery. The disk spool absorbs the failed publishes and the drainer
// replays them with their original Nats-Msg-Id once NATS returns.
//
// The == assertion is the whole point: fewer rows = the spool lost events
// (the pre-spool behavior — 146,757 dropped in one VM seizure), more rows =
// something double-delivered (the retired HTTP fallback produced ~1k
// duplicate impressions per NATS bounce; a spool replay outside the stream's
// Duplicates window did the same until it was widened to 30m).
func TestChaosNATSOutageSpoolLossless(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	harness.RequireKubectl(t)

	// A FRESH advertiser account scopes the count query to exactly this
	// test's impressions — no other traffic can pollute the == assertion.
	advEmail := fmt.Sprintf("chaos-spool-%d@e2e.test", time.Now().UnixNano())
	adv := h.Signup(t, "Chaos Spool Adv", advEmail, "pw-e2e-1", "advertiser")
	advID := accountIDByEmail(t, h, advEmail)

	const imps = 10
	h.WithChaos(t, "nats", func() {
		// Fired INTO the outage: every publish fails and must hit the spool.
		for i := 0; i < imps; i++ {
			h.FireImpression(t, fmt.Sprintf("%032x", time.Now().UnixNano()+int64(i)),
				"chaos-spool-camp", "chaos-spool-cr", "chaos-spool-pl", "chaos-spool-pub",
				advID, "USD", 2.50)
		}
	})

	// NATS is back (WithChaos waited for ready). The spool drains on a ~2s
	// tick and reporting consumes the replays; the count must converge to
	// EXACTLY imps.
	query := `{"table":"impressions","metrics":["count"],"time_from":"` +
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"}`
	count := func() float64 {
		res := h.APIJSON(t, adv, http.MethodPost, "/v1/api/reports", query)
		rows, _ := res["rows"].([]any)
		if len(rows) == 0 {
			return -1
		}
		row, _ := rows[0].([]any)
		if len(row) == 0 {
			return -1
		}
		n, _ := row[0].(float64)
		return n
	}
	harness.WaitFor(t, 120*time.Second, "spooled impressions to drain into reporting", func() bool {
		return count() == float64(imps)
	})
	// Duplicate guard: give any straggling replay time to land, then the
	// count must STILL be exactly imps.
	time.Sleep(5 * time.Second)
	if n := count(); n != float64(imps) {
		t.Fatalf("impression count moved after convergence: want exactly %d, got %v (loss<%d, dupes>%d)", imps, n, imps, imps)
	}
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
