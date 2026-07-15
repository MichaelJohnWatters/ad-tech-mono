//go:build e2e

// Household targeting (CTV, Phase 9 step 90) — end to end through the real
// stack: the SSP derives a household id from the client IP (salted HMAC,
// identity.HouseholdID), carries it as a user.eids entry, looks up PUBLIC
// household segments (audience members keyed by the hh: id) and stamps them;
// the DSP's targeting evaluator then matches campaigns whose
// include_segments name a household segment. Consent gates household
// personalisation exactly like user segments.
package e2e

import (
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestHouseholdTargeting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-household")

	// The member key is the household id for the test IP — derived with the
	// SAME function and the SAME (dev default) salt the SSP uses, so the ids
	// line up. A stack with a custom SSP_HOUSEHOLD_SALT would need the same
	// override here.
	const householdIP = "203.0.113.77"
	hhID := identity.HouseholdID(keys.SSP.HouseholdSalt.Default(), householdIP)

	segID := h.CreateSegment(t, w.AdvAcc, "tgt-hh-sports-fans")
	h.AddUserToSegment(t, segID, hhID)

	// Campaign bids only for that household segment; open geo so the
	// segment is the only gate.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	// BasicWorld targets device=mobile; open it up so the CTV request isn't
	// dropped on the device dimension (the segment is the only gate).
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})
	h.RefreshAllCaches(t)

	t.Run("household_ip_matches_even_anonymous", func(t *testing.T) {
		// No user_id, no uid2 — a fully anonymous CTV viewer. The household
		// id alone makes the segment match.
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "ctv", IP: householdIP,
		})
		if win := h.ExtractWinner(t, res); win.NoBid || win.CampaignID != w.Campaign.ID {
			t.Errorf("household viewer: winner=%+v, want campaign %s", win, w.Campaign.ID)
		}
	})

	t.Run("second_device_same_household_matches", func(t *testing.T) {
		// Different user, same IP → same household → same segment match.
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
			UserID: "hh-other-viewer", IP: householdIP,
		})
		if win := h.ExtractWinner(t, res); win.NoBid || win.CampaignID != w.Campaign.ID {
			t.Errorf("co-viewer: winner=%+v, want campaign %s", win, w.Campaign.ID)
		}
	})

	t.Run("different_ip_no_match", func(t *testing.T) {
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "ctv", IP: "203.0.113.99",
		})
		if win := h.ExtractWinner(t, res); !win.NoBid {
			t.Errorf("different household won: %+v (segment leaked across households)", win)
		}
	})

	t.Run("no_consent_no_household_personalisation", func(t *testing.T) {
		// GDPR applies + no TCF consent → privacy.Evaluate downgrades to
		// contextual-only; the DSP must not use the household segment.
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "DEU", Device: "ctv",
			IP: householdIP, GDPR: "1",
		})
		if win := h.ExtractWinner(t, res); !win.NoBid {
			t.Errorf("consent-less household request won: %+v (household targeting must be consent-gated)", win)
		}
	})
}
