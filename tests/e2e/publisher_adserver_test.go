//go:build e2e

// Publisher-adserver arbitration — verifies the direct-sold ladder runs
// correctly against the running stack. Each test sets up a placement, adds
// publisher line items in different tiers/configs, and asserts on the
// `source` field of the /v1/pubad/serve response. Covers the four critical
// cases from pkg/publisheradserver/arbitration that depend on real
// integration (warm cache loading from Postgres, NATS invalidate, SSP
// fall-through, ad server creative resolution):
//
//   1. Sponsorship wins over a guaranteed-behind-pace and a house line item.
//   2. No direct line items → SSP/exchange/DSP path runs (source=programmatic).
//   3. Paused direct line item is skipped → falls through to programmatic.
//   4. House line item fills when programmatic returns no-bid (geo mismatch).
//
// All four share BuildBasicWorld (which creates one publisher + one
// placement + one live programmatic campaign) and layer the direct-sold
// rows on top.
package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestPubAdSponsorshipWins — a sponsorship line item beats both a
// guaranteed line item and the programmatic auction. The arbiter is
// expected to short-circuit at the sponsorship tier without ever calling
// the SSP. We verify by checking source=direct, priority_tier=sponsorship,
// and that the served HTML carries our sponsorship marker.
func TestPubAdSponsorshipWins(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pa-spon")

	// Drop in both a guaranteed (behind pace by virtue of having a long flight
	// and zero served impressions) and a sponsorship. Sponsorship must win.
	flightStart := time.Now().Add(-1 * time.Hour)
	flightEnd := time.Now().Add(48 * time.Hour)

	_ = h.AddPublisherLineItem(t, w.Publisher, "pa-spon-guar-"+w.Publisher.ExternalID, harness.PubLineItemOpts{
		PriorityTier:         "guaranteed",
		DemandSource:         "TestGuarBuyer",
		Placements:           []harness.Placement{w.Placement},
		ImpressionsCommitted: 100000,
		DeliveryStart:        flightStart,
		DeliveryEnd:          flightEnd,
		CPM:                  4.00,
		CreativeHTML:         `<div data-tier="guaranteed">guar</div>`,
	})
	spon := h.AddPublisherLineItem(t, w.Publisher, "pa-spon-spon-"+w.Publisher.ExternalID, harness.PubLineItemOpts{
		PriorityTier:  "sponsorship",
		DemandSource:  "TestSponsorshipBuyer",
		Placements:    []harness.Placement{w.Placement},
		DeliveryStart: flightStart,
		DeliveryEnd:   flightEnd,
		CPM:           25.00,
		CreativeHTML:  `<div data-tier="sponsorship">spon-marker</div>`,
	})
	h.RefreshAllCaches(t)

	resp := h.ServePubAd(t, w.Placement.ExternalID)
	if resp.Source != "direct" {
		t.Fatalf("source: got %q, want direct (resp=%+v)", resp.Source, resp)
	}
	if resp.LineItemID != spon.ID {
		t.Errorf("LineItemID: got %q, want %q (sponsorship)", resp.LineItemID, spon.ID)
	}
	if resp.PriorityTier != "sponsorship" {
		t.Errorf("PriorityTier: got %q, want sponsorship", resp.PriorityTier)
	}
	if !strings.Contains(resp.HTML, "spon-marker") {
		t.Errorf("HTML missing sponsorship marker; got %s", resp.HTML)
	}
}

// TestPubAdNoDirectFallsThroughToProgrammatic — when no direct-sold line
// items exist for the placement, the publisher-adserver must call the SSP
// and return the programmatic winner. BuildBasicWorld already created a
// live GBR/mobile campaign that bids 3.50 against this placement's 1.00
// floor, so we expect a programmatic win (source=programmatic, html
// reflects the e2e campaign creative).
func TestPubAdNoDirectFallsThroughToProgrammatic(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pa-fall")

	// No publisher line items added. Cache refresh still needed in case any
	// stale rows linger from prior runs (Reset truncates, but RefreshAllCaches
	// also re-syncs the placement cache after BuildBasicWorld's inserts).
	h.RefreshAllCaches(t)

	// Pass geo/device the seed campaign targets so the programmatic auction
	// has a non-empty eligible-bid set. Without these the DSP correctly
	// no-bids and the test would degrade into a no-bid assertion instead of
	// a programmatic-fill one.
	resp := pubAdServeWithQuery(t, h, w.Placement.ExternalID, "GBR", "mobile")
	if resp.Source == "direct" {
		t.Fatalf("source: got direct, want programmatic (no direct line items expected)")
	}
	if resp.NoBid {
		t.Fatalf("expected programmatic fill, got nobid (resp=%+v)", resp)
	}
	// The SSP-passthrough response shape doesn't set `source`, so empty
	// means "passed through from SSP" which is the expected programmatic
	// fall-through. HTML should carry the e2e campaign marker.
	if !strings.Contains(resp.HTML, "via e2e") {
		t.Errorf("HTML missing programmatic e2e marker; got %s", resp.HTML)
	}
}

// TestPubAdPausedDirectSkipped — a paused sponsorship is skipped during
// arbitration; the request falls through to programmatic. Verifies that the
// status='paused' filter in the warm cache loader actually excludes the row.
func TestPubAdPausedDirectSkipped(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pa-paused")

	flightStart := time.Now().Add(-1 * time.Hour)
	flightEnd := time.Now().Add(48 * time.Hour)
	spon := h.AddPublisherLineItem(t, w.Publisher, "pa-paused-spon-"+w.Publisher.ExternalID, harness.PubLineItemOpts{
		PriorityTier:  "sponsorship",
		Placements:    []harness.Placement{w.Placement},
		DeliveryStart: flightStart,
		DeliveryEnd:   flightEnd,
		CPM:           25.00,
		CreativeHTML:  `<div data-tier="sponsorship">paused-marker</div>`,
	})
	h.SetPublisherLineItemStatus(t, spon, "paused")
	h.RefreshAllCaches(t)

	resp := pubAdServeWithQuery(t, h, w.Placement.ExternalID, "GBR", "mobile")
	if resp.Source == "direct" {
		t.Fatalf("source: got direct, want programmatic (sponsorship was paused)")
	}
	if strings.Contains(resp.HTML, "paused-marker") {
		t.Errorf("paused sponsorship still served; html=%s", resp.HTML)
	}
}

// TestPubAdHouseFillsOnNoBid — when programmatic returns no-bid (geo
// mismatch against the seed campaign's GBR-only targeting), the
// publisher-adserver falls through to a house line item. Verifies the
// programmatic-then-house fallback path in serveHandler.
func TestPubAdHouseFillsOnNoBid(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pa-house")

	house := h.AddPublisherLineItem(t, w.Publisher, "pa-house-h-"+w.Publisher.ExternalID, harness.PubLineItemOpts{
		PriorityTier: "house",
		DemandSource: "Publisher (house)",
		Placements:   []harness.Placement{w.Placement},
		CPM:          0,
		CreativeHTML: `<div data-tier="house">house-marker</div>`,
	})
	h.RefreshAllCaches(t)

	// USA forces a geo miss against the GBR-only programmatic campaign. The
	// SSP→exchange path returns nobid; the publisher-adserver should retry
	// with the house line item.
	resp := pubAdServeWithQuery(t, h, w.Placement.ExternalID, "USA", "desktop")
	if resp.Source != "direct" {
		t.Fatalf("source: got %q, want direct (house fallback)", resp.Source)
	}
	if resp.LineItemID != house.ID {
		t.Errorf("LineItemID: got %q, want %q (house)", resp.LineItemID, house.ID)
	}
	if resp.PriorityTier != "house" {
		t.Errorf("PriorityTier: got %q, want house", resp.PriorityTier)
	}
	if !strings.Contains(resp.HTML, "house-marker") {
		t.Errorf("HTML missing house marker; got %s", resp.HTML)
	}
}

// pubAdServeWithQuery is the geo/device-aware variant of harness.ServePubAd.
// Kept inline because only this file cares about the geo/device knobs (the
// rest of the suite uses placement-only serves). If a second file needs
// this, lift it into harness/publisher_adserver.go.
func pubAdServeWithQuery(t *testing.T, h *harness.Harness, placement, geo, device string) harness.PubAdServeResponse {
	t.Helper()
	// Build the URL and decode via the harness's plain ServePubAd by tacking
	// extra query params on. ServePubAd already URL-encodes via simple
	// concatenation (placement keys are ASCII external IDs); we extend the
	// same pattern.
	return h.ServePubAdRaw(t, "placement_id="+placement+"&geo="+geo+"&device="+device)
}
