//go:build e2e

// Derived segment rules (composite + lookalike) — the profile store's
// "later, same seam" kinds, end to end:
//
//   - composite: all_of[seed, other] intersects at person level;
//   - lookalike: a user whose behavioural category profile matches the
//     seed's enrolls; the seed itself and behaviour-less users don't.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDerivedRuleSegments(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "derived-rules")
	uniq := time.Now().UnixNano()
	uSeed := fmt.Sprintf("drv-seed-%d", uniq)
	uSimilar := fmt.Sprintf("drv-similar-%d", uniq)
	uOther := fmt.Sprintf("drv-other-%d", uniq)

	// The placement carries content categories so behaviour rows are
	// category-stamped (the lookalike's signal).
	if _, err := h.DB.Exec(`
UPDATE placements SET floor_config = jsonb_set(COALESCE(floor_config, '{}'::jsonb), '{categories}', '["sports","news"]')
WHERE id = $1::uuid`, w.Placement.ID); err != nil {
		t.Fatalf("set placement categories: %v", err)
	}
	h.RefreshAllCaches(t)

	// Seed + other segments (onboarded lists).
	segSeed := h.UploadAudience(t, w.AdvAcc.ID, fmt.Sprintf("drv-seedlist-%d", uniq), "public", []string{uSeed})
	segOther := h.UploadAudience(t, w.AdvAcc.ID, fmt.Sprintf("drv-otherlist-%d", uniq), "public", []string{uSeed, uOther})

	// Derived segments.
	var segComposite, segLookalike string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'composite', 'active', 'profile_builder', 'public',
        jsonb_build_object('kind', 'composite', 'all_of', jsonb_build_array($3::text, $4::text)))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("drv-composite-%d", uniq), segSeed, segOther).Scan(&segComposite); err != nil {
		t.Fatalf("create composite segment: %v", err)
	}
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'lookalike', 'active', 'profile_builder', 'public',
        jsonb_build_object('kind', 'lookalike', 'seed_segment', $3::text, 'min_similarity', 0.5))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("drv-lookalike-%d", uniq), segSeed).Scan(&segLookalike); err != nil {
		t.Fatalf("create lookalike segment: %v", err)
	}

	// Behaviour: the seed user AND the similar user browse the sports
	// placement; uOther generates nothing.
	for _, uid := range []string{uSeed, uSeed, uSimilar, uSimilar} {
		h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: uid,
		})
	}
	deadline := time.Now().Add(45 * time.Second)
	for h.LakeResidual(t, uSimilar)["behaviour_signals"] < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("behaviour rows never landed: %v", h.LakeResidual(t, uSimilar))
		}
		time.Sleep(2 * time.Second)
	}

	runBuilder(t, h, lakeStore(t, h))

	member := func(segID, userID string) bool {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`,
			segID, userID).Scan(&n); err != nil {
			t.Fatalf("membership query: %v", err)
		}
		return n > 0
	}

	// Composite = seed ∩ other = uSeed only.
	if !member(segComposite, uSeed) {
		t.Error("composite: uSeed (in both all_of segments) not enrolled")
	}
	if member(segComposite, uOther) {
		t.Error("composite: uOther (only in one all_of segment) enrolled")
	}

	// Lookalike: uSimilar (same category profile) in; the seed itself and
	// the behaviour-less uOther out.
	if !member(segLookalike, uSimilar) {
		t.Error("lookalike: uSimilar (matching category profile) not enrolled")
	}
	if member(segLookalike, uSeed) {
		t.Error("lookalike: seed user self-enrolled")
	}
	if member(segLookalike, uOther) {
		t.Error("lookalike: behaviour-less user enrolled")
	}

	// The composite gates a real auction.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segComposite})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})
	h.RefreshAllCaches(t)
	win := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: uSeed,
	}))
	if win.NoBid || win.CampaignID != w.Campaign.ID {
		t.Errorf("composite member auction: %+v, want campaign %s", win, w.Campaign.ID)
	}
	loser := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: uOther,
	}))
	if !loser.NoBid {
		t.Errorf("composite non-member won: %+v", loser)
	}
}
