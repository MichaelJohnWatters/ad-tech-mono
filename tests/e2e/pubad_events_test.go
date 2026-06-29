//go:build e2e

// Publisher-adserver event-emission tests. Verify the two new NATS
// subjects (adtech.direct.win + adtech.prebid.outbound.win) actually
// reach reporting, closing the analytics blind spots where direct-sold
// serves and external Prebid wins used to leave no record.
//
// Both tests follow the same shape as auction_win_test.go: run a serve,
// wait briefly for the NATS round-trip, assert the analytics store has
// a record tagged with the right BidModel.
package e2e

import (
	"database/sql"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestDirectWinEventLandsInAnalytics — direct-sold sponsorship serve must
// publish adtech.direct.win, reporting must consume and write to the
// analytics store with bid_model="direct:sponsorship". If this fails, the
// publisher-adserver isn't publishing OR reporting isn't subscribing.
func TestDirectWinEventLandsInAnalytics(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "evt-direct")

	flightStart := time.Now().Add(-1 * time.Hour)
	flightEnd := time.Now().Add(48 * time.Hour)
	spon := h.AddPublisherLineItem(t, w.Publisher, "evt-direct-spon", harness.PubLineItemOpts{
		PriorityTier:  "sponsorship",
		DemandSource:  "AcmeBrand",
		Placements:    []harness.Placement{w.Placement},
		DeliveryStart: flightStart,
		DeliveryEnd:   flightEnd,
		CPM:           18.50,
		CreativeHTML:  `<div data-test="direct-event">marker</div>`,
	})
	h.RefreshAllCaches(t)

	resp := h.ServePubAd(t, w.Placement.ExternalID)
	if resp.Source != "direct" {
		t.Fatalf("source = %q; want direct (test setup expected sponsorship win)", resp.Source)
	}
	if resp.LineItemID != spon.ID {
		t.Fatalf("served wrong line item: got %q, want %q", resp.LineItemID, spon.ID)
	}
	traceID := resp.TraceID

	// NATS publish is async (fire-and-forget goroutine in pubad). Poll up
	// to 5s for the event to reach reporting + land in the analytics store.
	harness.WaitFor(t, 5*time.Second, "direct.win event in analytics", func() bool {
		return h.AuctionWinByBidModel(t, traceID, "direct:sponsorship") >= 1
	})

	// Should be exactly one record — no duplicate publish path.
	time.Sleep(200 * time.Millisecond)
	got := h.AuctionWinByBidModel(t, traceID, "direct:sponsorship")
	if got != 1 {
		t.Errorf("direct:sponsorship records for trace %q = %d, want 1", traceID, got)
	}

	// And nothing should have been recorded as a programmatic auction win
	// (no exchange auction ran on the direct path).
	if other := h.AuctionWinByBidModel(t, traceID, "cpm"); other != 0 {
		t.Errorf("unexpected programmatic auction-win records on direct path: %d (trace %q)", other, traceID)
	}
}

// TestPrebidOutboundWinEventLandsInAnalytics — when an external Prebid
// Server wins over the SSP, pubad must publish adtech.prebid.outbound.win
// with bid_model="prebid_outbound" so analytics has visibility into
// money flowing through the publisher via external demand.
func TestPrebidOutboundWinEventLandsInAnalytics(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "evt-prebid")

	prebidStub := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode:     harness.FakeDSPBidder,
		BidPrice: 17.25,
		Seat:     "external-seat-x",
	})
	h.SetConfigForPod(t, "publisher_adserver.prebid_servers",
		prebidStub.URL+"/v1/openrtb/bid", "publisher-adserver-0")
	t.Cleanup(func() {
		h.SetConfigForPod(t, "publisher_adserver.prebid_servers", "", "publisher-adserver-0")
	})

	resp := h.ServePubAdRaw(t, "placement_id="+w.Placement.ExternalID+"&geo=GBR&device=mobile")
	if resp.Source != "prebid" {
		t.Fatalf("source = %q; want prebid (17.25 should beat SSP's ~3.50)", resp.Source)
	}
	traceID := resp.TraceID

	harness.WaitFor(t, 5*time.Second, "prebid.outbound.win event in analytics", func() bool {
		return h.AuctionWinByBidModel(t, traceID, "prebid_outbound") >= 1
	})

	time.Sleep(200 * time.Millisecond)
	got := h.AuctionWinByBidModel(t, traceID, "prebid_outbound")
	if got != 1 {
		t.Errorf("prebid_outbound records for trace %q = %d, want 1", traceID, got)
	}
}

// TestProgrammaticDirectAndPrebidEventsDistinctInAnalytics — sanity check
// that the BidModel tag actually distinguishes the three winning paths so
// analytics queries can filter cleanly. We run three serves under three
// trace IDs and verify each landed under its own bid_model bucket only.
func TestProgrammaticDirectAndPrebidEventsDistinctInAnalytics(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "evt-mixed")

	// Direct: add a sponsorship.
	flightStart := time.Now().Add(-1 * time.Hour)
	flightEnd := time.Now().Add(48 * time.Hour)
	_ = h.AddPublisherLineItem(t, w.Publisher, "evt-mixed-spon", harness.PubLineItemOpts{
		PriorityTier:  "sponsorship",
		Placements:    []harness.Placement{w.Placement},
		DeliveryStart: flightStart,
		DeliveryEnd:   flightEnd,
		CPM:           20.00,
		CreativeHTML:  `<div>spon</div>`,
	})

	// Prebid outbound: stub bidding higher than SSP.
	prebidStub := harness.NewFakeDSP(t, harness.FakeDSPOpts{
		Mode: harness.FakeDSPBidder, BidPrice: 22.00, Seat: "px",
	})
	h.SetConfigForPod(t, "publisher_adserver.prebid_servers",
		prebidStub.URL+"/v1/openrtb/bid", "publisher-adserver-0")
	t.Cleanup(func() {
		h.SetConfigForPod(t, "publisher_adserver.prebid_servers", "", "publisher-adserver-0")
	})
	h.RefreshAllCaches(t)

	// 1) Direct serve (sponsorship preempts before any auction).
	directResp := h.ServePubAd(t, w.Placement.ExternalID)
	if directResp.Source != "direct" {
		t.Fatalf("first serve source = %q; want direct", directResp.Source)
	}
	harness.WaitFor(t, 5*time.Second, "direct event landed", func() bool {
		return h.AuctionWinByBidModel(t, directResp.TraceID, "direct:sponsorship") >= 1
	})
	if c := h.AuctionWinByBidModel(t, directResp.TraceID, "prebid_outbound"); c != 0 {
		t.Errorf("direct trace has prebid_outbound records: %d", c)
	}

	// 2) Pause sponsorship so the next serve falls through to programmatic
	// where the Prebid stub will win over SSP. WithTenant sets the RLS
	// context so the UPDATE on publisher_line_items isn't blocked.
	h.WithTenant(t, w.PubAcc.ID, func(tx *sql.Tx) {
		if _, err := tx.Exec(`UPDATE publisher_line_items SET status='paused' WHERE name='evt-mixed-spon'`); err != nil {
			t.Fatalf("pause sponsorship: %v", err)
		}
	})
	h.RefreshAllCaches(t)

	prebidResp := h.ServePubAdRaw(t, "placement_id="+w.Placement.ExternalID+"&geo=GBR&device=mobile")
	if prebidResp.Source != "prebid" {
		t.Fatalf("second serve source = %q; want prebid (sponsorship paused, prebid 22 > ssp 3.50)", prebidResp.Source)
	}
	harness.WaitFor(t, 5*time.Second, "prebid event landed", func() bool {
		return h.AuctionWinByBidModel(t, prebidResp.TraceID, "prebid_outbound") >= 1
	})
	if c := h.AuctionWinByBidModel(t, prebidResp.TraceID, "direct:sponsorship"); c != 0 {
		t.Errorf("prebid trace has direct records: %d", c)
	}
}
