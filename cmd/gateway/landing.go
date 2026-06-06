package main

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
	Muted   string // secondary text colour
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
		Muted:   "#a8c4c0",
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
		Muted:   "#666",
	},
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
		Muted:   "#b5c2d4",
	}
}
