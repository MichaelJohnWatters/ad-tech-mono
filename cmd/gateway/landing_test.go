package main

import "testing"

func TestLandingThemeForSlug(t *testing.T) {
	t.Run("known slug returns its registered theme", func(t *testing.T) {
		for slug, want := range map[string]string{
			"luxauto":    "LuxAuto",
			"megastore":  "MegaStore",
			"quickbite":  "QuickBite",
			"acme-shoes": "Acme Shoes",
			"cryptoex":   "CryptoEx",
			"cloudcrm":   "CloudCRM",
			"vpnplus":    "VPN Plus",
			"epicquest":  "EpicQuest",
		} {
			if got := landingThemeForSlug(slug); got.Brand != want {
				t.Errorf("slug %q: Brand = %q, want %q", slug, got.Brand, want)
			}
		}
	})

	t.Run("unknown slug falls back to neutral default with slug as brand", func(t *testing.T) {
		// Future YAMLs may add new domains before landing.go grows
		// their themes. Falling back to a generic page keeps the demo
		// from 404'ing on those clicks.
		theme := landingThemeForSlug("unknown-brand")
		if theme.Brand != "unknown-brand" {
			t.Errorf("Brand = %q, want unknown-brand (slug echoed)", theme.Brand)
		}
		if theme.BG == "" || theme.FG == "" || theme.Accent == "" {
			t.Errorf("default theme must populate BG/FG/Accent: %+v", theme)
		}
	})

	t.Run("registered themes always have all palette fields", func(t *testing.T) {
		// Catch a config drift where someone adds a slug but forgets
		// to set, e.g., Accent — the page would render with an empty
		// CTA colour and look broken.
		for slug := range landingThemes {
			th := landingThemeForSlug(slug)
			if th.BG == "" || th.FG == "" || th.Accent == "" || th.Glyph == "" || th.CTA == "" {
				t.Errorf("theme %q has empty field(s): %+v", slug, th)
			}
		}
	})
}
