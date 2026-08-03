//go:build e2e

// Per-campaign channel targeting: an advertiser restricts a campaign to specific
// channels (include_channels). The DSP filters a bid when the request's channel
// isn't in a non-empty allowlist — so a DOOH-only campaign bids on DOOH requests
// but not on display. Empty allowlist = all channels (unchanged).
package e2e

import (
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCampaignChannelTargeting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "chan-tgt")

	// Restrict the campaign to DOOH only.
	setTargeting(t, h, w.Campaign.ID, "include_channels", pq.StringArray{"dooh"})
	h.RefreshAllCaches(t)

	// A plain display request: the DOOH-only campaign is not eligible → no bid.
	disp := h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", ""))
	if !disp.NoBid {
		t.Errorf("DOOH-only campaign bid on a display request: %+v", disp)
	}

	// A DOOH request: the channel is in the allowlist → bids and wins.
	dooh := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Channel: "dooh", Geo: "GBR", Device: "mobile",
	}))
	if dooh.NoBid || dooh.CampaignID != w.Campaign.ID {
		t.Errorf("DOOH request should be won by the DOOH-allowlisted campaign, got %+v", dooh)
	}

	// Widen the allowlist to include display → it bids on display again.
	setTargeting(t, h, w.Campaign.ID, "include_channels", pq.StringArray{"dooh", "display"})
	h.RefreshAllCaches(t)
	if h.ExtractWinner(t, h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "")).NoBid {
		t.Error("campaign with display in the allowlist did not bid on a display request")
	}
}
