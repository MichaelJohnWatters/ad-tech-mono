package main

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

func TestSelectCreativeForSize(t *testing.T) {
	c := &models.Campaign{
		CreativeID: "primary-uuid",
		Creatives: []models.CampaignCreative{
			{ID: "mpu", Width: 300, Height: 250},
			{ID: "leader", Width: 728, Height: 90},
			{ID: "halfpage", Width: 300, Height: 600},
		},
	}

	t.Run("exact size match wins", func(t *testing.T) {
		if got := selectCreativeForSize(c, 728, 90); got != "leader" {
			t.Errorf("got %q, want leader", got)
		}
	})

	t.Run("no matching size returns empty (skip bid)", func(t *testing.T) {
		// Pod auction logic relies on this — a campaign with no
		// size-matching creative no_bids for the request, which is the
		// cleanest signal "we don't have inventory for this slot".
		if got := selectCreativeForSize(c, 970, 250); got != "" {
			t.Errorf("got %q, want empty string for unmatched size", got)
		}
	})

	t.Run("zero request size falls back to primary CreativeID", func(t *testing.T) {
		// Non-display channels (video, audio) don't carry banner.w/h.
		// Falling back to CreativeID keeps them bidding instead of
		// no-bidding because of a size check that doesn't apply.
		if got := selectCreativeForSize(c, 0, 0); got != "primary-uuid" {
			t.Errorf("got %q, want primary-uuid", got)
		}
	})

	t.Run("empty creatives slice + zero size returns primary CreativeID", func(t *testing.T) {
		bare := &models.Campaign{CreativeID: "only-cr"}
		if got := selectCreativeForSize(bare, 0, 0); got != "only-cr" {
			t.Errorf("got %q, want only-cr", got)
		}
	})

	t.Run("empty creatives slice + sized request returns empty", func(t *testing.T) {
		// Migration safety: a Campaign loaded from the DB before the
		// multi-size schema landed has CreativeID but no Creatives. A
		// sized request to such a campaign must NOT match — the legacy
		// row implicitly was 300x250 only.
		bare := &models.Campaign{CreativeID: "only-cr"}
		if got := selectCreativeForSize(bare, 300, 250); got != "" {
			t.Errorf("got %q, want empty (no Creatives populated)", got)
		}
	})

	t.Run("only height mismatch", func(t *testing.T) {
		if got := selectCreativeForSize(c, 300, 251); got != "" {
			t.Errorf("got %q, want empty for off-by-one height", got)
		}
	})
}
