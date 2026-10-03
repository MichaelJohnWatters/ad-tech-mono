//go:build e2e

// Audience-segment visibility in the trace system. The SSP resolves the
// user's public audience segments and (consent-gated) stamps them on the bid
// request (user.ext.segments); the exchange carries them on AuctionWinEvent;
// reporting lands them in the auction_wins `segments` column; the trace
// timeline surfaces "Audience resolved: …" to staff + the winning advertiser.
//
// Negative (the enforcement surface): a GPC opt-out request must suppress the
// stamp at the SSP — the auction still runs contextually, but the durable win
// record carries NO segments and the trace shows no audience step. Publisher
// redaction (a publisher-scoped trace never shows segments) is unit-tested in
// cmd/reporting/trace_test.go.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAudienceSegmentsSurfaceInTrace(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "segtrace")

	// Stale SmartRouter stats (learned from whatever traffic preceded the
	// reset) can exclude the internal DSP from the display fan-out and flake
	// the win. Routing isn't what's under test — bypass it for the duration.
	t.Cleanup(func() { h.DeleteConfig(t, "exchange.routing_enabled") })
	h.SetConfigForPod(t, "exchange.routing_enabled", "false", harness.PodExchange)

	// Positive: default signals (no GDPR/GPC) allow personalisation, so the
	// explicit ?segments= (the documented test hook — unioned with lookups)
	// rides user.ext.segments into the auction. Poll: the kill-switch is a
	// live-config row the exchange picks up on its next poll tick.
	var tid string
	harness.WaitFor(t, 30*time.Second, "segment-stamped auction wins", func() bool {
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: "segtrace-u1", Segments: "sports_fans,auto_intenders",
		})
		if h.ExtractWinner(t, res).NoBid || res.TraceID == "" {
			return false
		}
		tid = res.TraceID
		return true
	})

	// The durable record: the win row carries the segments (retry for the
	// async NATS→ClickHouse write).
	harness.WaitFor(t, 40*time.Second, "auction_wins row carries the segments", func() bool {
		return h.ClickHouseScalar(t, fmt.Sprintf(
			"SELECT count() FROM adtech.auction_wins WHERE trace_id = '%s' AND has(segments, 'sports_fans') AND has(segments, 'auto_intenders')", tid)) > 0
	})

	// The staff trace timeline surfaces the step with the real segment ids,
	// sourced from that ClickHouse record.
	body := getTraceAsStaff(t, h, tid)
	if !strings.Contains(body, "Audience resolved") ||
		!strings.Contains(body, "sports_fans") || !strings.Contains(body, "auto_intenders") {
		t.Errorf("staff trace should show 'Audience resolved' with both segments; got:\n%s", body)
	}

	// Negative: GPC (opt-out) → the SSP's consent gate suppresses the stamp.
	// The auction still fills contextually; the win record has no segments
	// and the trace has no audience step — clean degrade, not an error.
	var tid2 string
	harness.WaitFor(t, 30*time.Second, "GPC auction wins contextually", func() bool {
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: "segtrace-u2", Segments: "sports_fans", GPC: "1",
		})
		if h.ExtractWinner(t, res).NoBid || res.TraceID == "" {
			return false
		}
		tid2 = res.TraceID
		return true
	})
	harness.WaitFor(t, 40*time.Second, "GPC win row lands in auction_wins", func() bool {
		return h.ClickHouseScalar(t, fmt.Sprintf(
			"SELECT count() FROM adtech.auction_wins WHERE trace_id = '%s'", tid2)) > 0
	})
	if n := h.ClickHouseScalar(t, fmt.Sprintf(
		"SELECT count() FROM adtech.auction_wins WHERE trace_id = '%s' AND notEmpty(segments)", tid2)); n > 0 {
		t.Errorf("GPC (opt-out) request leaked segments onto the win record")
	}
	body2 := getTraceAsStaff(t, h, tid2)
	if strings.Contains(body2, "Audience resolved") || strings.Contains(body2, "sports_fans") {
		t.Errorf("GPC trace must not show an audience step; got:\n%s", body2)
	}
}
