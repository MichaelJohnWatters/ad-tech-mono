//go:build e2e

// Analytics-gap closure tests. Each one proves that an event that used
// to vanish into a log line now reaches reporting:
//
//   Gap 1 — BudgetDepletedEvent (DSP → reporting)
//   Gap 4 — Video + Audio engagement (tracker → reporting)
//   Gap 5 — ServeNoFillEvent (publisher-adserver → reporting)
//
// All three follow the same shape: trigger the producing path, poll the
// reporting analytics store for the record, assert count >= 1.
package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// browserHeaders makes a request look like a real player/browser (UA + Referer)
// so the tracker's real-time fraud check doesn't drop it as a bot. Beacon-firing
// tests must set these — the tracker scores a Go-default UA + no Referer as
// fraud (bot_user_agent, no_referer) and rejects the event instead of recording
// it. Mirrors what cmd/simulator sends on its mirror path.
func browserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-e2e)")
	req.Header.Set("Referer", "https://e2e.dev/")
}

// TestBudgetDepletedEventReachesReporting — shrink a campaign's daily
// budget below ONE impression's realized cost, fire one auction (wins,
// consumes budget), fire a second (campaign now flagged exhausted, DSP
// publishes BudgetDepletedEvent). Reporting must have a depletion record
// for the campaign.
//
// Budget economics post money-precision: a ~3.50 CPM win books
// 3.50/1000 = $0.0035 against the daily budget, so exhausting a budget
// in one impression needs a budget under that (0.001 here). The old
// 1.00 budget would take ~300 impressions to deplete.
func TestBudgetDepletedEventReachesReporting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "evt-budget")

	h.SetCampaignDailyBudget(t, w.Campaign, 0.001)
	h.RefreshAllCaches(t)

	// First auction wins and exhausts the 0.001 budget. Fire the impression
	// too: a win WITHOUT an impression is a phantom win whose local budget
	// decrement the pacing reconcile (billing committed snapshot → DSP
	// counter) correctly releases — a snapshot tick landing between the two
	// auctions would un-deplete the campaign and flake this test. A billed
	// impression makes the spend real, so it survives reconciliation.
	first := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "evt-budget-1")
	win := h.ExtractWinner(t, first)
	if win.NoBid {
		t.Fatal("first auction should win — budget exists")
	}
	h.FireImpression(t, first.TraceID, win.CampaignID, win.CreativeID,
		first.PlacementID, first.PublisherID, w.AdvAcc.ID, "USD", win.Price)
	// Brief beat for the nurl callback + impression billing to register.
	time.Sleep(200 * time.Millisecond)

	// The DSP detects exhaustion at bid time and publishes the depleted
	// event; retry a few auctions in case the first raced the spend landing.
	harness.WaitFor(t, 10*time.Second, "budget depleted event recorded", func() bool {
		h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "evt-budget-2")
		return h.BudgetDepletionsByCampaign(t, w.Campaign.ID) >= 1
	})
}

// TestVideoTrackerEventReachesReporting — fire a /v1/t/video pixel with
// a known trace_id and event=start; reporting must record a media event
// tagged channel=video, event_type=start.
func TestVideoTrackerEventReachesReporting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	_ = harness.BuildBasicWorld(t, h, "evt-video")

	traceID := "evt-video-trace-001"
	// Sign the beacon like a real player does (the ad server hands out signed
	// video tracker URLs), so it validates whether or not the stack has strict
	// tracker.signature_validation on — matching production, not the lax dev
	// default.
	url := adserving.SignURL(h.URLs.Tracker+routes.TrackerVideo+"?tid="+traceID+"&event=start", adserving.DefaultSigningKey)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	browserHeaders(req) // a real player's UA/referer — else the tracker's fraud check drops the media event
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("video pixel: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("video pixel status: got %d, want 204", resp.StatusCode)
	}

	harness.WaitFor(t, 5*time.Second, "video event recorded", func() bool {
		return h.MediaEventsByTrace(t, traceID, "video", "start") >= 1
	})
}

// TestAudioTrackerEventReachesReporting — same but for /v1/t/audio.
func TestAudioTrackerEventReachesReporting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	_ = harness.BuildBasicWorld(t, h, "evt-audio")

	traceID := "evt-audio-trace-001"
	// Signed like production (see the video test) so it validates under strict
	// tracker.signature_validation too.
	url := adserving.SignURL(h.URLs.Tracker+routes.TrackerAudio+"?tid="+traceID+"&event=complete", adserving.DefaultSigningKey)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	browserHeaders(req) // see the video test — fraud check drops bot-UA media events
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("audio pixel: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("audio pixel status: got %d, want 204", resp.StatusCode)
	}

	harness.WaitFor(t, 5*time.Second, "audio event recorded", func() bool {
		return h.MediaEventsByTrace(t, traceID, "audio", "complete") >= 1
	})
}

// TestCampaignStateChangeReachesReporting — pause a campaign through the
// DSP management API; reporting must record the live → paused transition
// via adtech.campaign.state_changed. Then resume and assert the second
// transition lands as well. Asserts deltas because the analytics store
// accumulates across the whole reporting pod lifetime — earlier test runs
// leave records for the same deterministic campaign ID.
//
// Assertions count the SPECIFIC transition rather than inspecting the last
// record: the two flips happen <1s apart, and the ClickHouse read orders by a
// second-precision timestamp that can't disambiguate them — so "the last row"
// is ambiguous, but "a live→paused row appeared" is not.
func TestCampaignStateChangeReachesReporting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "evt-state")

	count := func(old, new string) int {
		n := 0
		for _, r := range h.CampaignStateChangesByCampaign(t, w.Campaign.ID) {
			if r.OldState == old && r.NewState == new {
				n++
			}
		}
		return n
	}
	startCount := len(h.CampaignStateChangesByCampaign(t, w.Campaign.ID))
	pausedBefore := count("live", "paused")
	resumedBefore := count("paused", "live")

	h.PatchCampaignStatus(t, w.Campaign, "paused")
	harness.WaitFor(t, 15*time.Second, "live → paused recorded", func() bool {
		return count("live", "paused") > pausedBefore
	})

	h.PatchCampaignStatus(t, w.Campaign, "live")
	harness.WaitFor(t, 15*time.Second, "paused → live recorded", func() bool {
		return count("paused", "live") > resumedBefore
	})

	// No-op patch (status already "live") must not generate a third event.
	h.PatchCampaignStatus(t, w.Campaign, "live")
	time.Sleep(500 * time.Millisecond)
	delta := len(h.CampaignStateChangesByCampaign(t, w.Campaign.ID)) - startCount
	if delta != 2 {
		t.Errorf("no-op patch generated extra event: delta = %d transitions, want 2", delta)
	}
}

// TestAdserverRenderFailedEventForUnknownCreative — request an ad with
// a creative_id the resolver can't find. The pixel still serves
// (browser sees a 200 with placeholder HTML, identical to a real
// creative) but reporting should record a render_failed event tagged
// reason=unknown_creative so ops can see "creative X is broken
// without log scraping."
func TestAdserverRenderFailedEventForUnknownCreative(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "evt-render")

	const fakeCreativeID = "00000000-0000-0000-0000-deadbeefcafe"
	startCount := len(h.RenderFailuresByCreative(t, fakeCreativeID))

	// ServeAd takes a campaign_id + creative_id directly. Use a
	// real campaign but an unresolvable creative — the resolver will
	// miss, fall back to placeholder HTML, and publish render_failed.
	if status := h.ServeAd(t, models.ServeRequest{
		TraceID:      "evt-render-trace-001",
		CampaignID:   w.Campaign.ID,
		CreativeID:   fakeCreativeID,
		PlacementID:  w.Placement.ID,
		PublisherID:  w.Publisher.ID,
		AdvertiserID: w.AdvAcc.ID,
		SiteDomain:   w.Publisher.Domain,
		Width:        300, Height: 250, Currency: "USD",
		UserID: "render-fail-user",
	}); status != http.StatusOK {
		t.Fatalf("ServeAd status = %d, want 200 (placeholder still rendered)", status)
	}

	harness.WaitFor(t, 5*time.Second, "render_failed event recorded", func() bool {
		return len(h.RenderFailuresByCreative(t, fakeCreativeID)) > startCount
	})
	records := h.RenderFailuresByCreative(t, fakeCreativeID)
	last := records[len(records)-1]
	if last.Reason != "unknown_creative" {
		t.Errorf("Reason = %q, want unknown_creative", last.Reason)
	}
	if last.CampaignID != w.Campaign.ID {
		t.Errorf("CampaignID = %q, want %q", last.CampaignID, w.Campaign.ID)
	}
}

// TestServeNoFillEventReachesReporting — request an ad where nothing
// can possibly fill (geo+device mismatch against the only campaign,
// no direct line items, no Prebid Servers configured). Pubad falls
// through every demand source and publishes ServeNoFillEvent.
func TestServeNoFillEventReachesReporting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "evt-nofill")

	// JPN/desktop will miss the BasicWorld campaign's GBR/mobile targeting.
	// No direct line items, no Prebid Servers → nothing fills.
	resp := h.ServePubAdRaw(t, "placement_id="+w.Placement.ExternalID+"&geo=JPN&device=desktop")
	if !resp.NoBid {
		t.Fatalf("expected nobid (geo mismatch); got %+v", resp)
	}
	traceID := resp.TraceID

	harness.WaitFor(t, 5*time.Second, "nofill event recorded", func() bool {
		return h.ServeNoFillsByTrace(t, traceID) >= 1
	})

	// Smoke: should NOT be recorded as an auction win.
	if c := h.AuctionWinCount(t, traceID); c != 0 {
		t.Errorf("trace %q recorded as auction win: %d", traceID, c)
	}
	// Reason string should mention the fall-through path.
	// (We only have a counter endpoint — can't inspect the reason. Skip.)
	_ = strings.Contains
}
