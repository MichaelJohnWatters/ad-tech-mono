//go:build e2e

// Targeting matrix — verifies the DSP's include/exclude rules actually
// gate bidding. Each test sets a single dimension and runs two auctions:
// one that matches (should win), one that doesn't (should NoBid).
//
// Helpers reach into targeting_rules directly with UPDATE so we don't
// need a "modify campaign" API to exist yet. The DSP warm cache picks
// up the change via RefreshAllCaches.
package e2e

import (
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestTargetingGeoInclude(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-geo-incl")

	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{"GBR"})
	h.RefreshAllCaches(t)

	assertWin(t, h, w.Placement.ExternalID, "GBR", "mobile", "geo-match")
	assertNoBid(t, h, w.Placement.ExternalID, "USA", "mobile", "geo-miss")
}

func TestTargetingGeoExclude(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-geo-excl")

	setTargeting(t, h, w.Campaign.ID, "exclude_geo", pq.StringArray{"USA"})
	// Clear include so the exclude is the only gate
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	h.RefreshAllCaches(t)

	assertWin(t, h, w.Placement.ExternalID, "GBR", "mobile", "excl-ok")
	assertNoBid(t, h, w.Placement.ExternalID, "USA", "mobile", "excl-blocked")
}

func TestTargetingDeviceInclude(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-dev-incl")

	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{"mobile"})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{}) // open geo
	h.RefreshAllCaches(t)

	assertWin(t, h, w.Placement.ExternalID, "GBR", "mobile", "dev-match")
	assertNoBid(t, h, w.Placement.ExternalID, "GBR", "desktop", "dev-miss")
}

func TestTargetingDomainExclude(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-dom-excl")

	// Exclude the publisher's own domain — auction must NoBid.
	setTargeting(t, h, w.Campaign.ID, "exclude_domains", pq.StringArray{w.Publisher.Domain})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	h.RefreshAllCaches(t)

	assertNoBid(t, h, w.Placement.ExternalID, "GBR", "mobile", "domain-blocked")
}

func TestTargetingCategoryInclude(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-cat-incl")

	// Placement is seeded with IAB12 (News). Including IAB12 = match.
	setTargeting(t, h, w.Campaign.ID, "include_categories", pq.StringArray{"IAB12"})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	h.RefreshAllCaches(t)
	assertWin(t, h, w.Placement.ExternalID, "GBR", "mobile", "cat-match")

	// Switch to IAB17 (Sports) — placement IAB12 → mismatch.
	setTargeting(t, h, w.Campaign.ID, "include_categories", pq.StringArray{"IAB17"})
	h.RefreshAllCaches(t)
	assertNoBid(t, h, w.Placement.ExternalID, "GBR", "mobile", "cat-miss")
}

// TestTargetingSegmentInclude — verifies the SSP→DSP audience segment path
// end to end: SSP looks up the user's segment memberships in Postgres,
// stamps them onto user.ext.segments on the outbound bid request, and the
// DSP's targeting evaluator drops campaigns whose include_segments don't
// intersect. Same shape as the geo/device tests, just exercising a
// different dimension.
func TestTargetingSegmentInclude(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-seg-incl")

	// Segment owned by the advertiser; one user in it, one out.
	segID := h.CreateSegment(t, w.AdvAcc, "tgt-seg-dog-owners")
	h.AddUserToSegment(t, segID, "user-with-segment")

	// Campaign only bids when the user matches the segment. Open geo so
	// the segment is the only gate.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	h.RefreshAllCaches(t)

	assertWin(t, h, w.Placement.ExternalID, "GBR", "mobile", "user-with-segment")
	assertNoBid(t, h, w.Placement.ExternalID, "GBR", "mobile", "user-without-segment")
}

// TestTargetingDSPPrivateSegment — verifies path B: the DSP enriches the
// bid request with segments from its own store (CRM lists, retargeting
// pixels, lookalikes) that the SSP never sees and never forwards.
//
// To prove the enrichment is real and not just path A re-run, the segment
// is created with visibility='dsp_private'. SSPSegmentsForUser filters
// these out; only DSPSegmentsForUser returns them. If the DSP's enrichment
// step is unwired, the bid request arrives with empty user.ext.segments
// and the campaign's include_segments rule drops the bid.
func TestTargetingDSPPrivateSegment(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-dsp-priv")

	// dsp_private segment: SSP will NOT stamp this; DSP must look it up.
	segID := h.CreateDSPPrivateSegment(t, w.AdvAcc, "tgt-dsp-priv-luxury-watches")
	h.AddUserToSegment(t, segID, "user-watch-shopper")

	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	h.RefreshAllCaches(t)

	assertWin(t, h, w.Placement.ExternalID, "GBR", "mobile", "user-watch-shopper")
	assertNoBid(t, h, w.Placement.ExternalID, "GBR", "mobile", "user-random")
}

// TestTargetingBidModifierApplied — a +50% device modifier should boost
// the bid for matching devices. Exchange wraps the auction at the
// adjusted price, so the clearing price reflects the modifier.
func TestTargetingBidModifierApplied(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "tgt-modifier")

	// Set a +100% mobile modifier. Base bid 3.50 → adjusted 7.00.
	if _, err := h.DB.Exec(`UPDATE targeting_rules
		SET bid_modifiers = '{"device":{"mobile":100}}'::jsonb, updated_at = now()
		WHERE line_item_id = $1`, w.Campaign.ID); err != nil {
		t.Fatalf("set modifier: %v", err)
	}
	h.RefreshAllCaches(t)

	res := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "modifier-user")
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Fatal("expected win with bid modifier applied; got no_bid")
	}
	// Base 3.50 + 100% modifier = 7.00. Allow a tiny tolerance for any
	// shading or float rounding.
	if win.Price < 6.50 {
		t.Errorf("clearing price = %.4f, expected ~7.00 with +100%% device modifier", win.Price)
	}
}

// --- helpers ---------------------------------------------------------------

func setTargeting(t *testing.T, h *harness.Harness, lineItemID, column string, value pq.StringArray) {
	t.Helper()
	// Tenant context isn't required for the table because we go through
	// the harness.DB directly with full perms; in prod admins would call
	// through Gateway which sets the tenant context.
	q := "UPDATE targeting_rules SET " + column + " = $1, updated_at = now() WHERE line_item_id = $2"
	if _, err := h.DB.Exec(q, value, lineItemID); err != nil {
		t.Fatalf("update %s: %v", column, err)
	}
}

func assertWin(t *testing.T, h *harness.Harness, placementExt, geo, device, user string) {
	t.Helper()
	res := h.RunAuction(t, placementExt, geo, device, user)
	win := h.ExtractWinner(t, res)
	if win.NoBid {
		t.Errorf("expected win for (geo=%s device=%s user=%s); got NoBid", geo, device, user)
	}
}

func assertNoBid(t *testing.T, h *harness.Harness, placementExt, geo, device, user string) {
	t.Helper()
	res := h.RunAuction(t, placementExt, geo, device, user)
	win := h.ExtractWinner(t, res)
	if !win.NoBid {
		t.Errorf("expected NoBid for (geo=%s device=%s user=%s); got winner %q at %.4f",
			geo, device, user, win.Seat, win.Price)
	}
}
