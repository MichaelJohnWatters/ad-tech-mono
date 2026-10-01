package main

import (
	"fmt"
	"html/template"
)

// chrome is the per-site visual identity + navigation. Each demo property
// (DEMOSITE_SITE slug) gets its own chrome so the sites stop looking like one
// uniform copy and instead read as their real-world counterpart (YouTube, Twitch,
// Spotify, Netflix, a newspaper, a tech blog). The ad PATH is unchanged — only the
// surrounding brand, palette, nav, and home surface differ.
type chrome struct {
	Theme themeSpec
	Nav   []navLink
	// Home is the template name the "/" route renders. Empty → fall back to the
	// Kind-based default (audio→audio.html, ssai→ssai.html, else home.html), which
	// keeps not-yet-bespoke properties rendering exactly as before.
	Home string
}

// themeSpec is the palette + typography a site paints with. Values flow into the
// stylesheet as CSS custom properties via siteConfig.ThemeCSS (trusted template.CSS
// — the values are our own constants, never user input).
type themeSpec struct {
	Accent    string // brand/action colour
	AccentInk string // text/icon colour on top of Accent
	Bg        string // page background
	Surface   string // cards / raised surfaces
	Ink       string // primary text
	Muted     string // secondary text
	Line      string // borders / dividers
	Font      string // font stack
	Wordmark  string // brand text shown in the top bar (may include a glyph)
	Dark      bool   // dark chrome (affects a few base rules)
}

// navLink is one top-bar entry; Key is matched against page.Active to highlight.
type navLink struct {
	Label string
	Href  string
	Key   string
}

// defaultChrome is the legacy "newspaper" look + the generic tab set. Any slug
// without a bespoke entry below renders with this, unchanged from before.
var defaultChrome = chrome{
	Theme: themeSpec{
		Accent: "#b91c1c", AccentInk: "#ffffff",
		Bg: "#faf9f7", Surface: "#ffffff", Ink: "#1a1a2e", Muted: "#6b7280", Line: "#e5e7eb",
		Font: "Georgia, 'Times New Roman', serif",
	},
	Nav: []navLink{
		{"Home", "/", "home"}, {"Video", "/video", "video"}, {"Audio", "/audio", "audio"},
		{"Native", "/native", "native"}, {"Pages", "/pages", "pages"},
	},
}

// siteChrome maps a pkg/simulator/pages Site slug to its bespoke chrome. Slugs
// absent here fall through to defaultChrome (see resolveChrome).
var siteChrome = map[string]chrome{
	// YouTube — light, red accent, system sans. Home is a watch-grid; the three
	// videos demonstrate pre-roll / pre+mid / pre+mid+post via VMAP.
	"viewtube": {
		Theme: themeSpec{
			Accent: "#ff0000", AccentInk: "#ffffff",
			Bg: "#f9f9f9", Surface: "#ffffff", Ink: "#0f0f0f", Muted: "#606060", Line: "#e5e5e5",
			Font:     "'Roboto', -apple-system, system-ui, sans-serif",
			Wordmark: "▶ ViewTube",
		},
		Nav: []navLink{
			{"Home", "/", "home"},
			{"Pre-roll", "/watch/1", "w1"},
			{"Pre + Mid", "/watch/2", "w2"},
			{"Pre + Mid + Post", "/watch/3", "w3"},
		},
		Home: "viewtube_home.html",
	},
	// Twitch — dark, purple accent. Home IS the live channel (continuous SSAI).
	"twitchr": {
		Theme: themeSpec{
			Accent: "#9147ff", AccentInk: "#ffffff",
			Bg: "#0e0e10", Surface: "#18181b", Ink: "#efeff1", Muted: "#adadb8", Line: "#2a2a2d",
			Font:     "'Inter', -apple-system, system-ui, sans-serif",
			Wordmark: "Twitchr", Dark: true,
		},
		Nav: []navLink{
			{"Live", "/", "live"},
			{"Browse", "/pages", "pages"},
		},
		Home: "twitchr_live.html",
	},
	// Spotify — dark, green accent. Home is a now-playing + playlist; the audio ad
	// spot plays before the track.
	"soundwave": {
		Theme: themeSpec{
			Accent: "#1db954", AccentInk: "#000000",
			Bg: "#121212", Surface: "#181818", Ink: "#ffffff", Muted: "#b3b3b3", Line: "#2a2a2a",
			Font:     "'Circular', -apple-system, system-ui, sans-serif",
			Wordmark: "◉ SoundWave", Dark: true,
		},
		Nav: []navLink{
			{"Home", "/", "home"},
			{"Your Library", "/pages", "pages"},
		},
		Home: "soundwave_home.html",
	},
	// Netflix — near-black, red accent. Home is a hero + rows of titles; a title
	// plays a pre-roll VAST then the content.
	"primereel": {
		Theme: themeSpec{
			Accent: "#e50914", AccentInk: "#ffffff",
			Bg: "#141414", Surface: "#1f1f1f", Ink: "#ffffff", Muted: "#b3b3b3", Line: "#2a2a2a",
			Font:     "'Netflix Sans', -apple-system, system-ui, sans-serif",
			Wordmark: "PRIMEREEL", Dark: true,
		},
		Nav: []navLink{
			{"Home", "/", "home"},
			{"My List", "/pages", "pages"},
		},
		Home: "primereel_home.html",
	},
	// Newspaper — light, serif, a real front page with in-article display + native.
	"chronicle": {
		Theme: themeSpec{
			Accent: "#8b0000", AccentInk: "#ffffff",
			Bg: "#faf9f7", Surface: "#ffffff", Ink: "#1a1a2e", Muted: "#6b7280", Line: "#d9d4cc",
			Font:     "Georgia, 'Times New Roman', serif",
			Wordmark: "The Daily Chronicle",
		},
		Nav: []navLink{
			{"Home", "/", "home"}, {"World", "/", "world"}, {"Business", "/", "business"},
			{"Technology", "/", "tech"}, {"Opinion", "/", "opinion"},
		},
		Home: "chronicle_home.html",
	},
	// Tech blog — light, teal accent, modern sans. Review feed + inline display/native.
	"gadget": {
		Theme: themeSpec{
			Accent: "#0d9488", AccentInk: "#ffffff",
			Bg: "#f8fafc", Surface: "#ffffff", Ink: "#0f172a", Muted: "#64748b", Line: "#e2e8f0",
			Font:     "-apple-system, 'Segoe UI', system-ui, sans-serif",
			Wordmark: "⚙ Gadget Grotto",
		},
		Nav: []navLink{
			{"Reviews", "/", "home"}, {"Deals", "/", "deals"},
			{"Phones", "/", "phones"}, {"Laptops", "/", "laptops"},
		},
		Home: "gadget_home.html",
	},
}

// resolveChrome returns the bespoke chrome for a slug, or the default.
func resolveChrome(slug string) chrome {
	if c, ok := siteChrome[slug]; ok {
		return c
	}
	return defaultChrome
}

// ThemeCSS renders the site's palette as a :root custom-property block. Returned
// as template.CSS (trusted) so html/template doesn't strip legitimate font stacks
// / colour values — safe because every value is a compile-time constant above.
func (c siteConfig) ThemeCSS() template.CSS {
	t := c.Theme
	return template.CSS(fmt.Sprintf(
		":root{--accent:%s;--accent-ink:%s;--bg:%s;--surface:%s;--ink:%s;--muted:%s;--line:%s;--font:%s;}",
		t.Accent, t.AccentInk, t.Bg, t.Surface, t.Ink, t.Muted, t.Line, t.Font,
	))
}
