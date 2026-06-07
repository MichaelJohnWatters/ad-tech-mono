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

func TestSelectCreativeForRequest_VideoMatching(t *testing.T) {
	c := &models.Campaign{
		Creatives: []models.CampaignCreative{
			{ID: "mpu", Format: "display", Width: 300, Height: 250},
			{ID: "vid-15s", Format: "video", Width: 640, Height: 360, Duration: 15, MediaURL: "https://x/15.mp4"},
			{ID: "vid-30s", Format: "video", Width: 640, Height: 360, Duration: 30, MediaURL: "https://x/30.mp4"},
		},
	}

	t.Run("video request within duration window picks first match", func(t *testing.T) {
		// MinDuration=5, MaxDuration=20 → only 15s creative fits.
		m := selectCreativeForRequest(c, "video", 0, 0, 5, 20)
		if m == nil || m.ID != "vid-15s" {
			t.Errorf("got %+v, want vid-15s", m)
		}
	})

	t.Run("video request with wide window picks first in order", func(t *testing.T) {
		// 1–60s window: both video creatives qualify; we take the
		// first in cache order (highest weight).
		m := selectCreativeForRequest(c, "video", 0, 0, 1, 60)
		if m == nil || m.ID != "vid-15s" {
			t.Errorf("got %+v, want vid-15s", m)
		}
	})

	t.Run("video duration below window → no match", func(t *testing.T) {
		// Window 31–60s excludes both video creatives.
		if m := selectCreativeForRequest(c, "video", 0, 0, 31, 60); m != nil {
			t.Errorf("got %+v, want nil (both video creatives below 31s min)", m)
		}
	})

	t.Run("display request ignores video creatives", func(t *testing.T) {
		m := selectCreativeForRequest(c, "display", 300, 250, 0, 0)
		if m == nil || m.ID != "mpu" {
			t.Errorf("got %+v, want mpu", m)
		}
	})

	t.Run("video creative missing MediaURL is skipped", func(t *testing.T) {
		// Defensive: a half-seeded video row (no media URL) must not
		// be returned as a bid candidate — the SSP would build a VAST
		// with an empty <MediaFile> and the player would fail.
		c2 := &models.Campaign{
			Creatives: []models.CampaignCreative{
				{ID: "bad-vid", Format: "video", Duration: 15, MediaURL: ""},
			},
		}
		if m := selectCreativeForRequest(c2, "video", 0, 0, 1, 30); m != nil {
			t.Errorf("got %+v, expected nil for video creative without MediaURL", m)
		}
	})

	t.Run("no creatives slice + display request + size 0 falls back to legacy ID", func(t *testing.T) {
		legacy := &models.Campaign{CreativeID: "only-cr"}
		m := selectCreativeForRequest(legacy, "display", 0, 0, 0, 0)
		if m == nil || m.ID != "only-cr" {
			t.Errorf("got %+v, want only-cr from CreativeID fallback", m)
		}
	})
}
