package main

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// TestDisplayAdMForBid: the cache-load projection already decided
// adm-eligibility (self-contained HTML populated, everything else empty) —
// this pins the lookup semantics: the PICKED creative's HTML, by id, and
// empty for non-eligible or unknown creatives.
func TestDisplayAdMForBid(t *testing.T) {
	selfContained := `<div onclick="window.open('https://acme.test')">ACME — 300x250</div>`
	c := &models.Campaign{Creatives: []models.CampaignCreative{
		{ID: "cr-macro", Format: "display"},                       // macro-carrying → HTML blank at load
		{ID: "cr-self", Format: "display", HTML: selfContained},   // adm-eligible
		{ID: "cr-video", Format: "video", MediaURL: "http://m/x"}, // wrong format → HTML blank
	}}

	if got := displayAdMForBid(c, "cr-self"); got != selfContained {
		t.Errorf("self-contained creative must emit its HTML, got %q", got)
	}
	if got := displayAdMForBid(c, "cr-macro"); got != "" {
		t.Errorf("macro-carrying creative must emit no adm, got %q", got)
	}
	if got := displayAdMForBid(c, "cr-video"); got != "" {
		t.Errorf("non-display creative must emit no adm, got %q", got)
	}
	if got := displayAdMForBid(c, "cr-unknown"); got != "" {
		t.Errorf("unknown creative must emit no adm, got %q", got)
	}
}
