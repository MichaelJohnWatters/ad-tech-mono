package main

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"

// displayAdMForBid returns the picked display creative's self-contained HTML
// for bid.adm (OpenRTB §4.3: display adm is HTML), or "" when the creative
// isn't adm-eligible. Eligibility was decided at cache load (SQL projection:
// format=display, inline html_content, no ${...} platform macros — see
// pkg/store/postgres/campaigns.go), so this is a pure in-memory lookup on
// the FINAL winning candidate only — same off-hot-loop discipline as
// buildMediaAdM. Macro-carrying / dynamic_product / asset-hosted creatives
// bid CreativeID-only: their markup is assembled by the ad server at render
// time and would be unrenderable standalone, so emitting it as adm would be
// dishonest to a third-party consumer.
//
// Same tracker decision as video: the adm carries NO DSP-side beacons —
// platform cost truth is the AuctionWinEvent, and the serve side injects
// the signed platform beacons (or renders internally when it knows the
// creative, which is the normal case for our own demand).
func displayAdMForBid(c *models.Campaign, creativeID string) string {
	for i := range c.Creatives {
		if c.Creatives[i].ID == creativeID {
			return c.Creatives[i].HTML
		}
	}
	return ""
}
