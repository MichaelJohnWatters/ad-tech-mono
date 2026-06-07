package main

import (
	"testing"
)

func TestBrandSlugFromDomain(t *testing.T) {
	cases := map[string]string{
		"":                 "default",
		"acme-shoes.com":   "acme-shoes",
		"globex-tech.com":  "globex-tech",
		"LuxAuto.com":      "luxauto",
		"single":           "single", // no dot — treated as slug as-is
	}
	for in, want := range cases {
		if got := brandSlugFromDomain(in); got != want {
			t.Errorf("brandSlugFromDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLandingURLFor(t *testing.T) {
	t.Run("with base routes to gateway slug", func(t *testing.T) {
		got := landingURLFor("http://localhost:8080/dev/landing", "luxauto.com")
		want := "http://localhost:8080/dev/landing/luxauto"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("trailing slash on base", func(t *testing.T) {
		got := landingURLFor("http://localhost:8080/dev/landing/", "luxauto.com")
		want := "http://localhost:8080/dev/landing/luxauto"
		if got != want {
			t.Errorf("trailing slash should be trimmed; got %q, want %q", got, want)
		}
	})
	t.Run("empty base falls back to https domain", func(t *testing.T) {
		got := landingURLFor("", "luxauto.com")
		if got != "https://luxauto.com" {
			t.Errorf("got %q, want https://luxauto.com", got)
		}
	})
	t.Run("empty base + empty domain → example.com", func(t *testing.T) {
		got := landingURLFor("", "")
		if got != "https://example.com" {
			t.Errorf("got %q, want https://example.com", got)
		}
	})
}

func TestUseAssetForSize(t *testing.T) {
	cases := []struct {
		w, h int
		want bool
	}{
		{300, 250, false}, // handcrafted MPU path
		{728, 90, true},
		{300, 600, true},
		{320, 50, true},
		{160, 600, true},
		{970, 250, true},
		{0, 0, true}, // anything that isn't 300x250 → asset path
	}
	for _, c := range cases {
		if got := useAssetForSize(c.w, c.h); got != c.want {
			t.Errorf("useAssetForSize(%d,%d) = %v, want %v", c.w, c.h, got, c.want)
		}
	}
}

func TestMaterialiseCreatives_LegacyShape(t *testing.T) {
	// Old YAMLs carry creative_id + creative_domain only — synthesise a
	// single 300x250 entry so they continue to work without rewriting
	// every existing line item.
	c := CampaignConfig{
		ID:             "li-001",
		CreativeID:     "cr-shoes-001",
		CreativeDomain: "acme-shoes.com",
	}
	out := materialiseCreatives(c)
	if len(out) != 1 {
		t.Fatalf("expected 1 creative, got %d", len(out))
	}
	cv := out[0]
	if cv.ID != "cr-shoes-001" || cv.Width != 300 || cv.Height != 250 {
		t.Errorf("legacy creative not materialised: %+v", cv)
	}
	if cv.Domain != "acme-shoes.com" {
		t.Errorf("domain = %q, want acme-shoes.com", cv.Domain)
	}
}

func TestMaterialiseCreatives_NewShape(t *testing.T) {
	// New YAMLs carry creatives:[{id,width,height,domain?}]. Each
	// entry becomes one row; missing dimensions default to 300x250
	// (the registered IAB MPU) so partial entries don't blow up.
	c := CampaignConfig{
		ID:             "li-001",
		CreativeDomain: "acme-shoes.com",
		Creatives: []CreativeYAML{
			{ID: "cr-shoes-001", Width: 300, Height: 250},
			{ID: "cr-shoes-001-leader", Width: 728, Height: 90},
			{Width: 320, Height: 50}, // ID omitted → auto-named
			{ID: "with-bad-dims"},   // Width/Height = 0 → default to 300x250
		},
	}
	out := materialiseCreatives(c)
	if len(out) != 4 {
		t.Fatalf("expected 4 creatives, got %d", len(out))
	}
	if out[0].ID != "cr-shoes-001" {
		t.Errorf("[0].ID = %q, want cr-shoes-001", out[0].ID)
	}
	if out[2].ID != "li-001-320x50" {
		t.Errorf("[2].ID auto-name = %q, want li-001-320x50", out[2].ID)
	}
	if out[3].Width != 300 || out[3].Height != 250 {
		t.Errorf("[3] missing dims should default to 300x250, got %dx%d", out[3].Width, out[3].Height)
	}
	for i, cv := range out {
		if cv.Domain != "acme-shoes.com" {
			t.Errorf("[%d].Domain inherited wrong: %q", i, cv.Domain)
		}
	}
}

func TestMaterialiseCreatives_BothShapes_ArrayWins(t *testing.T) {
	// When a YAML carries both legacy creative_id AND the new
	// creatives:[] array, the explicit array wins — the legacy
	// fields are ignored. Matches the documented precedence.
	c := CampaignConfig{
		ID:             "li-001",
		CreativeID:     "legacy-cr",
		CreativeDomain: "acme-shoes.com",
		Creatives: []CreativeYAML{
			{ID: "new-cr", Width: 728, Height: 90},
		},
	}
	out := materialiseCreatives(c)
	if len(out) != 1 || out[0].ID != "new-cr" {
		t.Errorf("array should win over legacy field, got %+v", out)
	}
}

func TestMaterialiseCreatives_Empty(t *testing.T) {
	// No legacy ID and no Creatives → nothing to insert. The caller
	// loops over the empty slice and skips.
	if out := materialiseCreatives(CampaignConfig{ID: "li-001"}); len(out) != 0 {
		t.Errorf("empty input should return empty slice, got %+v", out)
	}
}
