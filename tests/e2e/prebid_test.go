//go:build e2e

// Prebid Server-compatible bidder endpoint — verifies that external Prebid
// Server instances can POST OpenRTB 2.x bid requests to
// /v1/prebid/openrtb2/auction and get back a valid OpenRTB BidResponse from
// our exchange (after our floor policy is applied).
//
// Wire format is identical to /v1/openrtb/auction; the value-add at the
// Prebid endpoint is floor enforcement + opaque deal-id logging. Unit
// tests in pkg/prebid cover the floor math; this file covers the live
// wire path through cmd/exchange/prebid.go → auctionHandler.
//
// Bidder code we exposed: "adtechmono" (see pkg/prebid.BidderCode).
package e2e

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestPrebidInboundAuctionFills — a Prebid request that matches our
// BasicWorld campaign (GBR + mobile + the placement) should win the
// auction. Asserts that the bidder endpoint actually runs the full auction
// path and returns a valid OpenRTB-shape response. If this fails, either
// the translation layer broke the request shape or the loopback through
// auctionHandler isn't wired.
func TestPrebidInboundAuctionFills(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pb-fill")

	req := openrtb.BidRequest{
		ID: "pb-fill-trace",
		Imp: []openrtb.Imp{{
			ID:       "imp-1",
			TagID:    w.Placement.ID,
			BidFloor: 0.50,
			Banner:   &openrtb.Banner{W: 300, H: 250},
		}},
		Site: &openrtb.Site{
			Domain:    w.Publisher.Domain,
			Publisher: &openrtb.Publisher{ID: w.Publisher.ID},
		},
		Device: &openrtb.Device{
			Geo:        &openrtb.Geo{Country: "GBR"},
			DeviceType: 1, // mobile (matches BuildBasicWorld targeting)
		},
		TMax: 500,
	}
	resp, status := h.PostPrebidAuction(t, req)
	if status != 200 {
		t.Fatalf("prebid status: got %d, want 200", status)
	}
	if resp.NoBid {
		t.Fatalf("expected winning bid, got nobid (response=%+v)", resp)
	}
	if len(resp.SeatBid) == 0 || len(resp.SeatBid[0].Bid) == 0 {
		t.Fatalf("expected at least one seatbid/bid, got: %+v", resp)
	}
	bid := resp.SeatBid[0].Bid[0]
	if bid.Price <= 0 {
		t.Errorf("bid price: got %v, want > 0", bid.Price)
	}
	// BuildBasicWorld's campaign bids 3.50 CPM and floor was 0.50, so the
	// clearing price must land between them.
	if bid.Price < 0.50 || bid.Price > 3.50 {
		t.Errorf("bid price %v out of expected range [0.50, 3.50]", bid.Price)
	}
}

// TestPrebidInboundBelowFloorReturnsNoBid — when imp.BidFloor exceeds what
// any DSP would pay, the auction returns no_bid. Confirms the Prebid
// endpoint honours the inbound floor (whether ours or the publisher's,
// max-wins) and doesn't silently downgrade it. BuildBasicWorld's campaign
// bids 3.50 CPM; we set the inbound floor to 10.00 to guarantee no bid
// clears it.
func TestPrebidInboundBelowFloorReturnsNoBid(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pb-floor")

	req := openrtb.BidRequest{
		ID: "pb-floor-trace",
		Imp: []openrtb.Imp{{
			ID:       "imp-1",
			TagID:    w.Placement.ID,
			BidFloor: 10.00, // above what any DSP would bid
			Banner:   &openrtb.Banner{W: 300, H: 250},
		}},
		Site: &openrtb.Site{
			Domain:    w.Publisher.Domain,
			Publisher: &openrtb.Publisher{ID: w.Publisher.ID},
		},
		Device: &openrtb.Device{
			Geo:        &openrtb.Geo{Country: "GBR"},
			DeviceType: 1,
		},
		TMax: 500,
	}
	resp, status := h.PostPrebidAuction(t, req)
	if status != 200 {
		t.Fatalf("prebid status: got %d, want 200", status)
	}
	if !resp.NoBid && len(resp.SeatBid) > 0 {
		t.Fatalf("expected nobid (floor 10.00 above campaign max bid 3.50); got winning bid %+v", resp.SeatBid[0].Bid)
	}
}

// TestPrebidInboundDealIDPreserved — deal IDs on inbound impressions are
// passed through opaquely (no internal matching, per the locked decision
// in PLAN.md). We assert that supplying a deal_id doesn't break the
// request — the auction still runs and returns a normal response.
func TestPrebidInboundDealIDPreserved(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pb-deal")

	req := openrtb.BidRequest{
		ID: "pb-deal-trace",
		Imp: []openrtb.Imp{{
			ID:       "imp-1",
			TagID:    w.Placement.ID,
			BidFloor: 0.50,
			DealID:   "external-prebid-deal-xyz", // arbitrary opaque ID
			Banner:   &openrtb.Banner{W: 300, H: 250},
		}},
		Site: &openrtb.Site{
			Domain:    w.Publisher.Domain,
			Publisher: &openrtb.Publisher{ID: w.Publisher.ID},
		},
		Device: &openrtb.Device{
			Geo:        &openrtb.Geo{Country: "GBR"},
			DeviceType: 1,
		},
		TMax: 500,
	}
	_, status := h.PostPrebidAuction(t, req)
	if status != 200 {
		t.Fatalf("prebid status: got %d, want 200", status)
	}
	// The deal ID is logged but not matched against internal pkg/deals
	// (per the design). No assertion on the response shape — just that
	// the request was accepted and the auction ran.
}

// TestPrebidFloorPolicyAppliesPlatformMin — set prebid.min_bid_floor to
// 10.00 via the live config API, send an inbound request with
// bidfloor=0.50, assert nobid. Without the policy taking effect, the
// campaign's 3.50 bid would win. The fact that we get nobid proves the
// platform min raised the effective floor above what any DSP bids.
//
// Cleanup restores the default (0) so subsequent tests aren't suppressed.
func TestPrebidFloorPolicyAppliesPlatformMin(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pb-policy-floor")

	h.SetConfigForPod(t, "prebid.min_bid_floor", "10.0", "exchange-0")
	t.Cleanup(func() {
		h.SetConfigForPod(t, "prebid.min_bid_floor", "0", "exchange-0")
	})

	req := openrtb.BidRequest{
		ID: "pb-policy-floor-trace",
		Imp: []openrtb.Imp{{
			ID:       "imp-1",
			TagID:    w.Placement.ID,
			BidFloor: 0.50, // inbound floor < platform min → policy should raise to 10.0
			Banner:   &openrtb.Banner{W: 300, H: 250},
		}},
		Site: &openrtb.Site{
			Domain:    w.Publisher.Domain,
			Publisher: &openrtb.Publisher{ID: w.Publisher.ID},
		},
		Device: &openrtb.Device{
			Geo:        &openrtb.Geo{Country: "GBR"},
			DeviceType: 1,
		},
		TMax: 500,
	}
	resp, status := h.PostPrebidAuction(t, req)
	if status != 200 {
		t.Fatalf("prebid status: got %d, want 200", status)
	}
	if !resp.NoBid && len(resp.SeatBid) > 0 {
		t.Fatalf("expected nobid (platform min 10.0 should suppress 3.50 bid); got winning bid %+v", resp.SeatBid[0].Bid)
	}
}

// TestPrebidEndpointDisabled — toggle prebid.enabled=false, POST a request,
// expect 503. Restore to true on cleanup so other tests still work.
func TestPrebidEndpointDisabled(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pb-disabled")

	h.SetConfigForPod(t, "prebid.enabled", "false", "exchange-0")
	t.Cleanup(func() {
		h.SetConfigForPod(t, "prebid.enabled", "true", "exchange-0")
	})

	req := openrtb.BidRequest{
		ID: "pb-disabled-trace",
		Imp: []openrtb.Imp{{
			ID:       "imp-1",
			TagID:    w.Placement.ID,
			BidFloor: 0.50,
			Banner:   &openrtb.Banner{W: 300, H: 250},
		}},
		Site: &openrtb.Site{
			Domain:    w.Publisher.Domain,
			Publisher: &openrtb.Publisher{ID: w.Publisher.ID},
		},
		TMax: 500,
	}
	_, status := h.PostPrebidAuction(t, req)
	if status != 503 {
		t.Fatalf("expected 503 (endpoint disabled), got %d", status)
	}
}
