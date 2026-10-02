package main

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
)

// Themed mock landing pages for the seeded demo creatives. The tracker
// click handler redirects to /dev/landing/{slug}; this table maps the
// slug to a brand name, palette, glyph, tagline, and CTA so the page
// visually matches the creative the user clicked. Same theme axes
// (retail, tech, auto, finance, gaming, privacy, default) cmd/seed
// uses for the creative templates, so a click on a tech-themed ad
// lands on a tech-themed page.

type landingTheme struct {
	Brand   string
	Glyph   string
	Tagline string
	CTA     string
	BG      string // hero background colour (CSS)
	FG      string // primary text colour
	Accent  string // CTA button colour
	CTAFg   string // CTA button TEXT colour — must contrast with Accent (not BG,
	// which is a gradient and can't be used as a text colour)
	Muted string // secondary text colour
	// ConvValue is the headline purchase value the landing "Complete purchase" CTA
	// books (and shows). 0 → the generic demoConvertRevenue. Set it per brand so a
	// Ford landing books $38,000, not a $49.99 "car".
	ConvValue float64
}

// PurchaseValue is the revenue the landing demo-purchase books for this theme.
func (t landingTheme) PurchaseValue() float64 {
	if t.ConvValue > 0 {
		return t.ConvValue
	}
	return demoConvertRevenue
}

// PurchaseLabel formats PurchaseValue for the CTA ("$38,000" / "$49.99").
func (t landingTheme) PurchaseLabel() string {
	v := t.PurchaseValue()
	if v == math.Trunc(v) {
		s := strconv.FormatInt(int64(v), 10)
		var b strings.Builder
		for i, c := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(c)
		}
		return "$" + b.String()
	}
	return fmt.Sprintf("$%.2f", v)
}

var landingThemes = map[string]landingTheme{
	"luxauto": {
		Brand:   "LuxAuto",
		Glyph:   "🚗",
		Tagline: "Performance redefined for the open road.",
		CTA:     "Book a test drive",
		BG:      "linear-gradient(135deg,#1a1a2e,#0f3460)",
		FG:      "#fff",
		Accent:  "#e94560",
		CTAFg:   "#fff",
		Muted:   "#aab3d1",
	},
	"megastore": {
		Brand:   "MegaStore",
		Glyph:   "🛍️",
		Tagline: "Summer blowout — up to 40% off this week only.",
		CTA:     "Shop the sale",
		BG:      "linear-gradient(135deg,#ff7043,#ff5252)",
		FG:      "#fff",
		Accent:  "#fff",
		CTAFg:   "#b4232a",
		Muted:   "rgba(255,255,255,0.75)",
	},
	"quickbite": {
		Brand:   "QuickBite",
		Glyph:   "🥡",
		Tagline: "Food delivered fast. Install the app and get $10 off your first order.",
		CTA:     "Get the app",
		BG:      "linear-gradient(135deg,#ff7043,#ff5252)",
		FG:      "#fff",
		Accent:  "#fff",
		CTAFg:   "#b4232a",
		Muted:   "rgba(255,255,255,0.75)",
	},
	"acme-shoes": {
		Brand:   "Acme Shoes",
		Glyph:   "👟",
		Tagline: "Run further. Feel better. Free returns for 90 days.",
		CTA:     "Shop new arrivals",
		BG:      "linear-gradient(135deg,#ff7043,#ff5252)",
		FG:      "#fff",
		Accent:  "#fff",
		CTAFg:   "#b4232a",
		Muted:   "rgba(255,255,255,0.75)",
	},
	"cryptoex": {
		Brand:   "CryptoEx",
		Glyph:   "💱",
		Tagline: "Trade crypto with institutional-grade execution.",
		CTA:     "Open an account",
		BG:      "linear-gradient(135deg,#0b132b,#1c2541)",
		FG:      "#fff",
		Accent:  "#5bc0be",
		CTAFg:   "#071a33",
		Muted:   "#9ba9c9",
	},
	"cloudcrm": {
		Brand:   "CloudCRM",
		Glyph:   "⚡",
		Tagline: "Built for engineering teams. Stop tracking customers in spreadsheets.",
		CTA:     "Start free trial",
		BG:      "linear-gradient(135deg,#4361ee,#3a0ca3)",
		FG:      "#fff",
		Accent:  "#fff",
		CTAFg:   "#2a0a73",
		Muted:   "rgba(255,255,255,0.78)",
	},
	"initech": {
		Brand:   "Initech",
		Glyph:   "🧰",
		Tagline: "Enterprise SaaS that engineers actually like.",
		CTA:     "Request a demo",
		BG:      "linear-gradient(135deg,#4361ee,#3a0ca3)",
		FG:      "#fff",
		Accent:  "#fff",
		CTAFg:   "#2a0a73",
		Muted:   "rgba(255,255,255,0.78)",
	},
	"globex-tech": {
		Brand:   "Globex Tech",
		Glyph:   "🛰️",
		Tagline: "Cloud infrastructure for the next decade.",
		CTA:     "View pricing",
		BG:      "linear-gradient(135deg,#4361ee,#3a0ca3)",
		FG:      "#fff",
		Accent:  "#fff",
		CTAFg:   "#2a0a73",
		Muted:   "rgba(255,255,255,0.78)",
	},
	"epicquest": {
		Brand:   "EpicQuest",
		Glyph:   "🎮",
		Tagline: "The mobile RPG everyone's talking about. 4.8★ on the App Store.",
		CTA:     "Download free",
		BG:      "linear-gradient(135deg,#240046,#9d4edd)",
		FG:      "#fff",
		Accent:  "#ff6d00",
		CTAFg:   "#2a1500",
		Muted:   "#d6c2e8",
	},
	"vpnplus": {
		Brand:   "VPN Plus",
		Glyph:   "🛡️",
		Tagline: "Protect every device on every network. 7-day free trial.",
		CTA:     "Start protecting",
		BG:      "linear-gradient(135deg,#264653,#2a9d8f)",
		FG:      "#fff",
		Accent:  "#e9c46a",
		CTAFg:   "#1b3a3a",
		Muted:   "#a8c4c0",
	},
	"ford": {
		Brand:     "Ford",
		Glyph:     "🚙",
		Tagline:   "Built Ford Tough.",
		CTA:       "Build & Price",
		BG:        "linear-gradient(135deg,#00142e,#002a5c)",
		FG:        "#fff",
		Accent:    "#0276b3",
		CTAFg:     "#fff",
		Muted:     "#9fb6d4",
		ConvValue: 38000, // an F-150, not a $49.99 "car"
	},
	// Direct-sold + house demand sources.
	"acme": {
		Brand:   "Acme Corp",
		Glyph:   "🏢",
		Tagline: "Welcome to the Acme sponsorship destination.",
		CTA:     "Learn more",
		BG:      "linear-gradient(135deg,#1a1a2e,#16213e)",
		FG:      "#fff",
		Accent:  "#4cc9f0",
		CTAFg:   "#063048",
		Muted:   "#aab3d1",
	},
	"globex": {
		Brand:   "Globex Industries",
		Glyph:   "🏭",
		Tagline: "Guaranteed reach, guaranteed delivery.",
		CTA:     "Contact sales",
		BG:      "linear-gradient(135deg,#0a0a1a,#1a1a2e)",
		FG:      "#fff",
		Accent:  "#5bc0be",
		CTAFg:   "#062a2a",
		Muted:   "#9ba9c9",
	},
	"daily-news": {
		Brand:   "Daily News Newsletter",
		Glyph:   "📰",
		Tagline: "Subscribe and get tomorrow's headlines in your inbox tonight.",
		CTA:     "Subscribe now",
		BG:      "linear-gradient(135deg,#f5f5f5,#e0e0e0)",
		FG:      "#1a1a2e",
		Accent:  "#4361ee",
		CTAFg:   "#fff",
		Muted:   "#666",
	},
}

// StyleVars renders the theme's :root custom properties as trusted template.CSS.
// Must be template.CSS, not a plain string interpolated per-property: html/template
// BLANKS a linear-gradient value to "ZgotmplZ" in a CSS context, which made
// --bg invalid → the page fell back to a white background with white --fg text
// (unreadable). The values are compile-time constants, so trusting them is safe.
func (t landingTheme) StyleVars() template.CSS {
	return template.CSS(fmt.Sprintf(
		"--bg:%s;--fg:%s;--accent:%s;--cta-fg:%s;--muted:%s;",
		t.BG, t.FG, t.Accent, t.CTAFg, t.Muted,
	))
}

// landingThemeForSlug returns the theme record for the slug, falling
// back to a neutral default so an unknown slug still renders something.
func landingThemeForSlug(slug string) landingTheme {
	if t, ok := landingThemes[slug]; ok {
		return t
	}
	return landingTheme{
		Brand:   slug,
		Glyph:   "🌐",
		Tagline: "Mock landing destination served by the gateway.",
		CTA:     "Learn more",
		BG:      "linear-gradient(135deg,#2c3e50,#4a6491)",
		FG:      "#fff",
		Accent:  "#74c0fc",
		CTAFg:   "#0b2545",
		Muted:   "#b5c2d4",
	}
}
