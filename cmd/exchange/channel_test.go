package main

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// TestChannelForRequest guards the format-aware-routing fix: the exchange must
// derive the smart-router channel from the request's imp format, not a static
// knob. Routing on a blended "all" bucket let display auctions drag a
// native/audio-only DSP's bid rate below the drop threshold and starve those
// formats — so this mapping is load-bearing.
func TestChannelForRequest(t *testing.T) {
	cases := []struct {
		name string
		imp  openrtb.Imp
		want string
	}{
		{"video", openrtb.Imp{Video: &openrtb.Video{}}, "video"},
		{"audio", openrtb.Imp{Audio: &openrtb.Audio{}}, "audio"},
		{"native", openrtb.Imp{Native: &openrtb.Native{}}, "native"},
		{"banner", openrtb.Imp{Banner: &openrtb.Banner{}}, "display"},
		{"empty imp", openrtb.Imp{}, "display"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := channelForRequest(&openrtb.BidRequest{Imp: []openrtb.Imp{c.imp}})
			if got != c.want {
				t.Errorf("channelForRequest(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}

	// No imps at all → display (never panic on an empty request).
	if got := channelForRequest(&openrtb.BidRequest{}); got != "display" {
		t.Errorf("no imps: got %q, want display", got)
	}
}

// TestPlacementPublisherFromReq covers the keys a no-bid auction is recorded
// with, so the auctions table can be attributed to a placement/publisher the
// same way a won auction is (needed for accurate fill rate).
func TestPlacementPublisherFromReq(t *testing.T) {
	t.Run("prefers TagID + Publisher.ID (the UUIDs the SSP sets)", func(t *testing.T) {
		req := &openrtb.BidRequest{
			Imp:  []openrtb.Imp{{ID: "imp-1", TagID: "pl-uuid"}},
			Site: &openrtb.Site{Domain: "news.example", Publisher: &openrtb.Publisher{ID: "pub-uuid"}},
		}
		pl, pub := placementPublisherFromReq(req)
		if pl != "pl-uuid" {
			t.Errorf("placement = %q, want pl-uuid (Imp.TagID)", pl)
		}
		if pub != "pub-uuid" {
			t.Errorf("publisher = %q, want pub-uuid (Site.Publisher.ID)", pub)
		}
	})

	t.Run("falls back to Imp.ID + Site.Domain", func(t *testing.T) {
		req := &openrtb.BidRequest{
			Imp:  []openrtb.Imp{{ID: "imp-1"}},
			Site: &openrtb.Site{Domain: "news.example"},
		}
		pl, pub := placementPublisherFromReq(req)
		if pl != "imp-1" {
			t.Errorf("placement = %q, want imp-1 (fallback)", pl)
		}
		if pub != "news.example" {
			t.Errorf("publisher = %q, want news.example (fallback)", pub)
		}
	})

	t.Run("empty request doesn't panic", func(t *testing.T) {
		pl, pub := placementPublisherFromReq(&openrtb.BidRequest{})
		if pl != "" || pub != "" {
			t.Errorf("empty request: got (%q, %q), want empty", pl, pub)
		}
	})
}
