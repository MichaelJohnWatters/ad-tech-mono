//go:build e2e

// Consent gate on audience signal leaving the platform: the SSP must NOT stamp a
// user's public audience segments (user.ext.segments) onto a bid request sent to
// EXTERNAL DSPs when the request carries a do-not-sell/share signal (GPC). Our
// own DSP re-checks consent before USING segments, but an external buyer can't be
// relied on to — so no personalisation consent → no audience membership leaves
// the platform. A regression here silently discloses segment membership to every
// third-party buyer on an opted-out request; only an end-to-end check (reading
// what the external DSP actually received) catches it.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSSPConsentGatesSegmentsToExternalDSP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "consentseg")

	// External DSP records every bid request the exchange fans to it.
	fake := harness.NewFakeDSP(t, harness.FakeDSPOpts{Mode: harness.FakeDSPBidder, BidPrice: 8.0})
	h.SetConfigForPod(t, "exchange.dsp_endpoints", fake.URL+","+h.URLs.ClusterDSP, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	// Two members of the SAME public segment: one consented auction, one GPC.
	consented := fmt.Sprintf("consent-yes-%d", time.Now().UnixNano())
	optedOut := fmt.Sprintf("consent-gpc-%d", time.Now().UnixNano())
	h.UploadAudience(t, w.AdvAcc.ID, "consent-seg", "public", []string{consented, optedOut})
	h.RefreshAllCaches(t)

	// received returns the most recent bid request the external DSP got for user.
	received := func(user string) (openrtb.BidRequest, bool) {
		for _, br := range fake.BidRequests() {
			if br.User != nil && br.User.ID == user {
				return br, true
			}
		}
		return openrtb.BidRequest{}, false
	}

	// CONSENT GIVEN: run auctions until the external DSP receives a request for
	// the user WITH the segment stamped (retries cover cold caches / the SSP's
	// best-effort 25ms segment lookup — same warmup the datafee test relies on).
	var gotConsented bool
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && !gotConsented {
		h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: consented,
		})
		if br, ok := received(consented); ok && br.User.Ext != nil && len(br.User.Ext.Segments) > 0 {
			gotConsented = true
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !gotConsented {
		t.Fatal("with consent, the external DSP never received user.ext.segments — segments should ride (control failed)")
	}

	// GPC (do-not-sell/share): the SAME public segment must NOT leave the
	// platform. Run several auctions so the external DSP definitely gets one for
	// this user, then assert nothing rode.
	for i := 0; i < 8; i++ {
		h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: optedOut, GPC: "1",
		})
		time.Sleep(300 * time.Millisecond)
	}
	br, ok := received(optedOut)
	if !ok {
		t.Fatal("external DSP never received a request for the GPC user — cannot assert the gate")
	}
	if br.User.Ext != nil && len(br.User.Ext.Segments) > 0 {
		t.Errorf("GPC request LEAKED user.ext.segments to an external DSP: %v (consent gate failed)", br.User.Ext.Segments)
	}
	if len(br.User.Data) > 0 {
		t.Errorf("GPC request LEAKED user.data to an external DSP: %v", br.User.Data)
	}
}
