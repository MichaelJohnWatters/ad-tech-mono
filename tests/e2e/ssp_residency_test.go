//go:build e2e

// Data residency (PLAN Phase 11 #111), data-plane slice: when a bid request asserts
// regs.ext.data_residency for a region that is NOT this deployment's home region
// (platform.region), the SSP must NOT let the user's audience membership leave the
// platform (user.ext.segments / user.data) — the same suppression the consent gate
// applies, extended to residency. Bidding still proceeds; only user-level data is
// withheld. Only reading what the external DSP actually received proves it.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSSPResidencyGatesUserDataToExternalDSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "residencyseg")

	// External DSP records every bid request the exchange fans to it.
	fake := harness.NewFakeDSP(t, harness.FakeDSPOpts{Mode: harness.FakeDSPBidder, BidPrice: 8.0})
	h.SetConfigForPod(t, "exchange.dsp_endpoints", fake.URL+","+h.URLs.ClusterDSP, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	// Two members of the SAME public segment: one in-region, one out-of-region.
	inRegion := fmt.Sprintf("res-home-%d", time.Now().UnixNano())
	outRegion := fmt.Sprintf("res-eu-%d", time.Now().UnixNano())
	h.UploadAudience(t, w.AdvAcc.ID, "residency-seg", "public", []string{inRegion, outRegion})
	h.RefreshAllCaches(t)

	received := func(user string) (openrtb.BidRequest, bool) {
		for _, br := range fake.BidRequests() {
			if br.User != nil && br.User.ID == user {
				return br, true
			}
		}
		return openrtb.BidRequest{}, false
	}

	// CONTROL: no residency assertion (home region) → segments ride. Retry to
	// cover cold caches / the SSP's best-effort segment lookup.
	var gotHome bool
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && !gotHome {
		h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: inRegion,
		})
		if br, ok := received(inRegion); ok && br.User.Ext != nil && len(br.User.Ext.Segments) > 0 {
			gotHome = true
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !gotHome {
		t.Fatal("in-region: external DSP never received user.ext.segments — segments should ride (control failed)")
	}

	// OUT-OF-REGION: data_residency=eu (deployment home is us-east-1) → the SAME
	// public segment must NOT leave the platform. Run several so the external DSP
	// definitely gets one for this user, then assert nothing user-level rode.
	for i := 0; i < 8; i++ {
		h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: outRegion, DataResidency: "eu",
		})
		time.Sleep(300 * time.Millisecond)
	}
	br, ok := received(outRegion)
	if !ok {
		t.Fatal("external DSP never received a request for the out-of-region user — cannot assert the gate")
	}
	if br.User != nil && br.User.Ext != nil && len(br.User.Ext.Segments) > 0 {
		t.Errorf("out-of-region request LEAKED user.ext.segments to an external DSP: %v (residency gate failed)", br.User.Ext.Segments)
	}
	if br.User != nil && len(br.User.Data) > 0 {
		t.Errorf("out-of-region request LEAKED user.data to an external DSP: %v", br.User.Data)
	}
	// The residency flag itself DOES propagate downstream (regs.ext.data_residency),
	// so a region-local DSP could enforce too.
	if br.Regs == nil || br.Regs.Ext == nil || br.Regs.Ext.DataResidency != "eu" {
		t.Errorf("regs.ext.data_residency not propagated to the DSP: %+v", br.Regs)
	}
}
