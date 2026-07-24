//go:build e2e

// Segtax gate — standard-taxonomy audience data reaches an EXTERNAL buyer.
//
// The claim under test: labelling a PUBLIC segment with an IAB Audience
// Taxonomy node (migration 062) makes it ride the bid request to external
// bidders as OpenRTB user.data with ext.segtax=4 — the standards-
// interoperable form a third-party DSP can actually interpret — while
// consent gating holds at the SSP (a GDPR request with no consent string
// must carry NO user.data, because an external buyer can't be trusted to
// enforce our consent decision downstream).
//
// Path proven: portal label (gateway PUT /v1/api/audiences/taxonomy) →
// audience_segments.taxonomy_id → SSP taxonomy warm map → user.data on the
// outbound OpenRTB → exchange fan-out → an external endpoint OUTSIDE the
// cluster (FakeDSP via host.docker.internal, the same network path as
// cmd/extbidder).
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSegtaxRidesToExternalBidder(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "segtax")

	// External buyer: never bids (BidPrice 0), only records what it was sent.
	// Winning is not the claim — receipt of interpretable audience data is.
	fake := harness.NewFakeDSP(t, harness.FakeDSPOpts{Mode: harness.FakeDSPBidder})
	h.SetConfigForPod(t, "exchange.dsp_endpoints",
		fake.URL+","+h.URLs.ClusterDSP, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	// A public segment with one member, labelled "Interest | Automotive |
	// Auto Body Styles | SUV" (node 13 in the seeded taxonomy subset).
	const taxonomyID = 13
	member := fmt.Sprintf("segtax-member-%d", time.Now().UnixNano())
	segID := h.UploadAudience(t, w.AdvAcc.ID, "segtax-suv-intenders", "public", []string{member})
	h.SetSegmentTaxonomy(t, w.AdvAcc.ID, segID, taxonomyID)
	// Refresh pushes both the membership preload AND the SSP taxonomy map.
	h.RefreshAllCaches(t)

	// Consented run (no regulatory signals → full personalisation). The
	// taxonomy map lands on every SSP pod via the audience invalidate
	// (published by the taxonomy PUT) — asynchronous, so retry briefly
	// rather than assuming the first auction hits a refreshed pod.
	var got *openrtb.BidRequest
	consented := 0
	deadline := time.Now().Add(15 * time.Second)
	for got == nil && time.Now().Before(deadline) {
		h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: member,
		})
		consented = 0
		for _, br := range fake.BidRequests() {
			if br.User == nil || br.User.ID != member || br.Regs != nil {
				continue
			}
			consented++
			if len(br.User.Data) > 0 {
				b := br
				got = &b
				break
			}
		}
		if got == nil {
			time.Sleep(time.Second)
		}
	}
	if got == nil {
		if consented == 0 {
			t.Fatalf("external fake DSP never received the consented bid request (BidCalls=%d) — dsp_endpoints swap or fan-out broken", fake.BidCalls())
		}
		t.Fatalf("user.data missing on all %d consented receipts — taxonomy stamp did not fire (labelled public segment + member + consent)", consented)
	}

	// The internal segment id still rides user.ext.segments (platform-only)…
	foundInternal := false
	if got.User.Ext != nil {
		for _, s := range got.User.Ext.Segments {
			if s == segID {
				foundInternal = true
			}
		}
	}
	if !foundInternal {
		t.Errorf("user.ext.segments missing internal segment id %s — SSP segment stamping regressed", segID)
	}

	// …and the taxonomy label rides user.data with segtax 4.
	d := got.User.Data[0]
	if d.Ext == nil || d.Ext.Segtax != openrtb.SegtaxIABAudience11 {
		t.Errorf("user.data[0].ext.segtax = %+v, want %d (IAB Audience Taxonomy 1.1)", d.Ext, openrtb.SegtaxIABAudience11)
	}
	wantID := fmt.Sprintf("%d", taxonomyID)
	foundNode := false
	for _, s := range d.Segment {
		if s.ID == wantID {
			foundNode = true
		}
	}
	if !foundNode {
		t.Errorf("user.data[0].segment %+v missing taxonomy node id %q", d.Segment, wantID)
	}

	// Consent gate: GDPR applies + no TCF consent string → contextual only —
	// the SAME user and segment must produce NO user.data for external eyes.
	h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: member, GDPR: "1",
	})
	sawGated := false
	for _, br := range fake.BidRequests() {
		if br.User == nil || br.User.ID != member || br.Regs == nil || br.Regs.Ext == nil || br.Regs.Ext.GDPR != 1 {
			continue
		}
		sawGated = true
		if len(br.User.Data) != 0 {
			t.Errorf("GDPR-no-consent request carried user.data %+v — consent gate at the SSP failed", br.User.Data)
		}
	}
	if !sawGated {
		t.Error("external fake DSP never received the GDPR-flagged request — negative half of the consent assertion did not run")
	}
}
