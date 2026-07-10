// Programmatic themed SVG generator. Hand-crafting 6 themes × 6 sizes =
// 36 SVGs is busywork; generating them from a small theme catalog keeps
// the look consistent and makes adding a new size or theme cheap. The
// existing handcrafted themes/*-300x250.svg files stay as reference
// renderings; the generator uses the same palette so the generated and
// handcrafted versions read as the same brand family.
//
// Layouts are aspect-ratio driven:
//   - wide  (w/h > 4)        — leaderboard / mobile banner / billboard
//   - tall  (h/w > 1.5)      — skyscraper / half-page
//   - square-ish otherwise   — MPU / large rectangle
//
// Each layout picks a sensible glyph size + text wrapping so the same
// theme reads at 320×50 and at 970×250 without overflowing or looking
// lost in whitespace.
package main

import (
	"fmt"
	"strings"
)

type svgTheme struct {
	Slug      string
	BG1, BG2  string // gradient stops
	FG        string
	Accent    string
	Glyph     string // emoji-ish character drawn as large text
	Brand     string
	Tagline   string
	CTA       string
	CTAFG     string
	CTABG     string
}

var svgThemes = []svgTheme{
	{
		Slug: "retail", BG1: "#ff7043", BG2: "#ff5252",
		FG: "#fff", Accent: "#fff",
		Glyph: "🛍", Brand: "MEGASTORE",
		Tagline: "Up to 40% off this week",
		CTA:     "Shop Now", CTAFG: "#ff5252", CTABG: "#fff",
	},
	{
		Slug: "tech", BG1: "#4361ee", BG2: "#3a0ca3",
		FG: "#fff", Accent: "#4cc9f0",
		Glyph: "⚡", Brand: "CLOUDCRM",
		Tagline: "Built for engineering teams",
		CTA:     "Start Free Trial", CTAFG: "#3a0ca3", CTABG: "#fff",
	},
	{
		Slug: "auto", BG1: "#1a1a2e", BG2: "#0f3460",
		FG: "#fff", Accent: "#e94560",
		Glyph: "🚗", Brand: "LUXAUTO",
		Tagline: "Performance redefined",
		CTA:     "Book Test Drive", CTAFG: "#fff", CTABG: "#e94560",
	},
	{
		Slug: "finance", BG1: "#0b132b", BG2: "#1c2541",
		FG: "#fff", Accent: "#5bc0be",
		Glyph: "💱", Brand: "CRYPTOEX",
		Tagline: "Institutional execution",
		CTA:     "Open Account", CTAFG: "#0b132b", CTABG: "#5bc0be",
	},
	{
		Slug: "gaming", BG1: "#240046", BG2: "#9d4edd",
		FG: "#fff", Accent: "#ff6d00",
		Glyph: "🎮", Brand: "EPICQUEST",
		Tagline: "4.8★ mobile RPG",
		CTA:     "Download Free", CTAFG: "#fff", CTABG: "#ff6d00",
	},
	{
		Slug: "privacy", BG1: "#264653", BG2: "#2a9d8f",
		FG: "#fff", Accent: "#e9c46a",
		Glyph: "🛡", Brand: "VPN PLUS",
		Tagline: "Protect every device",
		CTA:     "Start Trial", CTAFG: "#264653", CTABG: "#e9c46a",
	},
}

func themeBySlug(slug string) svgTheme {
	for _, t := range svgThemes {
		if t.Slug == slug {
			return t
		}
	}
	return svgThemes[0]
}

// generateThemedSVG produces an SVG of the requested size styled with
// the theme's palette + glyph + brand. Layout is picked from the aspect
// ratio so the same theme reads correctly at any IAB standard size.
func generateThemedSVG(t svgTheme, w, h int) []byte {
	aspect := float64(w) / float64(h)
	switch {
	case aspect > 4: // leaderboard / mobile banner / billboard
		return generateWideSVG(t, w, h)
	case aspect < 0.6: // skyscraper / half-page
		return generateTallSVG(t, w, h)
	default: // MPU / large rectangle / square-ish
		return generateBlockSVG(t, w, h)
	}
}

// svgPrelude is the gradient + rounded background shared by every layout.
// Keeping it inlined per-SVG so each file is self-contained (no external
// stylesheets) and works when loaded as a standalone <img src=...>.
func svgPrelude(t svgTheme, w, h int) string {
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d">
  <defs>
    <linearGradient id="bg" x1="0" x2="1" y1="0" y2="1">
      <stop offset="0" stop-color="%s"/>
      <stop offset="1" stop-color="%s"/>
    </linearGradient>
  </defs>
  <rect width="%d" height="%d" fill="url(#bg)" rx="6"/>`, w, h, w, h, t.BG1, t.BG2, w, h)
}

// generateWideSVG is the horizontal layout: glyph on the left, brand +
// tagline + CTA stacked on the right. Used for leaderboards (728x90),
// mobile banners (320x50), and billboards (970x250).
func generateWideSVG(t svgTheme, w, h int) []byte {
	var b strings.Builder
	b.WriteString(svgPrelude(t, w, h))
	glyphSize := h - 16
	if glyphSize > 80 {
		glyphSize = 80
	}
	textX := glyphSize + 20
	brandSize := h / 4
	if brandSize > 32 {
		brandSize = 32
	}
	if brandSize < 14 {
		brandSize = 14
	}
	taglineSize := brandSize - 6
	if taglineSize < 10 {
		taglineSize = 10
	}
	ctaW := w / 4
	if ctaW > 160 {
		ctaW = 160
	}
	ctaX := w - ctaW - 16
	ctaY := (h - 36) / 2
	if ctaY < 8 {
		ctaY = 8
	}
	ctaH := 36
	if ctaH > h-16 {
		ctaH = h - 16
	}

	// Glyph + text
	fmt.Fprintf(&b, `
  <text x="%d" y="%d" font-family="Apple Color Emoji,Segoe UI Emoji,sans-serif" font-size="%d" dominant-baseline="middle">%s</text>`,
		20, h/2, glyphSize, t.Glyph)
	fmt.Fprintf(&b, `
  <text x="%d" y="%d" font-family="-apple-system,Segoe UI,sans-serif" font-weight="700" font-size="%d" fill="%s">%s</text>`,
		textX, h/2-2, brandSize, t.FG, t.Brand)
	if h >= 60 {
		fmt.Fprintf(&b, `
  <text x="%d" y="%d" font-family="-apple-system,Segoe UI,sans-serif" font-size="%d" fill="%s" opacity="0.85">%s</text>`,
			textX, h/2+brandSize-4, taglineSize, t.FG, t.Tagline)
	}
	// CTA pill
	if w >= 480 {
		fmt.Fprintf(&b, `
  <rect x="%d" y="%d" width="%d" height="%d" rx="%d" fill="%s"/>
  <text x="%d" y="%d" text-anchor="middle" font-family="-apple-system,Segoe UI,sans-serif" font-weight="600" font-size="14" fill="%s" dominant-baseline="middle">%s</text>`,
			ctaX, ctaY, ctaW, ctaH, ctaH/2, t.CTABG,
			ctaX+ctaW/2, ctaY+ctaH/2, t.CTAFG, t.CTA)
	}
	b.WriteString(`
</svg>`)
	return []byte(b.String())
}

// generateTallSVG is the vertical layout: glyph at top, brand + tagline
// + CTA stacked below. Used for skyscrapers (160x600) and half-pages
// (300x600).
func generateTallSVG(t svgTheme, w, h int) []byte {
	var b strings.Builder
	b.WriteString(svgPrelude(t, w, h))
	glyphSize := w * 3 / 5
	if glyphSize > 110 {
		glyphSize = 110
	}
	brandSize := w / 7
	if brandSize > 26 {
		brandSize = 26
	}
	if brandSize < 14 {
		brandSize = 14
	}
	taglineSize := brandSize - 6
	if taglineSize < 10 {
		taglineSize = 10
	}
	ctaH := 40
	ctaW := w - 32
	ctaY := h - ctaH - 20

	fmt.Fprintf(&b, `
  <text x="%d" y="%d" font-family="Apple Color Emoji,Segoe UI Emoji,sans-serif" font-size="%d" text-anchor="middle">%s</text>`,
		w/2, h/3, glyphSize, t.Glyph)
	fmt.Fprintf(&b, `
  <text x="%d" y="%d" text-anchor="middle" font-family="-apple-system,Segoe UI,sans-serif" font-weight="700" font-size="%d" fill="%s">%s</text>`,
		w/2, h/3+glyphSize/2+20, brandSize, t.FG, t.Brand)
	fmt.Fprintf(&b, `
  <text x="%d" y="%d" text-anchor="middle" font-family="-apple-system,Segoe UI,sans-serif" font-size="%d" fill="%s" opacity="0.85">%s</text>`,
		w/2, h/3+glyphSize/2+20+brandSize+4, taglineSize, t.FG, t.Tagline)
	fmt.Fprintf(&b, `
  <rect x="16" y="%d" width="%d" height="%d" rx="%d" fill="%s"/>
  <text x="%d" y="%d" text-anchor="middle" font-family="-apple-system,Segoe UI,sans-serif" font-weight="600" font-size="14" fill="%s" dominant-baseline="middle">%s</text>`,
		ctaY, ctaW, ctaH, ctaH/2, t.CTABG,
		w/2, ctaY+ctaH/2, t.CTAFG, t.CTA)
	b.WriteString(`
</svg>`)
	return []byte(b.String())
}

// generateBlockSVG is the centred layout: glyph above brand + tagline +
// CTA, all vertically stacked. Used for MPUs (300x250) and large
// rectangles (336x280).
func generateBlockSVG(t svgTheme, w, h int) []byte {
	var b strings.Builder
	b.WriteString(svgPrelude(t, w, h))
	glyphSize := h * 2 / 5
	if glyphSize > 90 {
		glyphSize = 90
	}
	brandSize := h / 8
	if brandSize > 24 {
		brandSize = 24
	}
	if brandSize < 14 {
		brandSize = 14
	}
	taglineSize := brandSize - 6
	if taglineSize < 10 {
		taglineSize = 10
	}
	ctaH := 36
	ctaW := w * 2 / 3
	if ctaW > 200 {
		ctaW = 200
	}
	ctaX := (w - ctaW) / 2
	ctaY := h - ctaH - 20

	fmt.Fprintf(&b, `
  <text x="%d" y="%d" font-family="Apple Color Emoji,Segoe UI Emoji,sans-serif" font-size="%d" text-anchor="middle">%s</text>`,
		w/2, h/2-glyphSize/4, glyphSize, t.Glyph)
	fmt.Fprintf(&b, `
  <text x="%d" y="%d" text-anchor="middle" font-family="-apple-system,Segoe UI,sans-serif" font-weight="700" font-size="%d" fill="%s">%s</text>`,
		w/2, h/2+glyphSize/4+10, brandSize, t.FG, t.Brand)
	fmt.Fprintf(&b, `
  <text x="%d" y="%d" text-anchor="middle" font-family="-apple-system,Segoe UI,sans-serif" font-size="%d" fill="%s" opacity="0.85">%s</text>`,
		w/2, h/2+glyphSize/4+10+brandSize+4, taglineSize, t.FG, t.Tagline)
	fmt.Fprintf(&b, `
  <rect x="%d" y="%d" width="%d" height="%d" rx="%d" fill="%s"/>
  <text x="%d" y="%d" text-anchor="middle" font-family="-apple-system,Segoe UI,sans-serif" font-weight="600" font-size="14" fill="%s" dominant-baseline="middle">%s</text>`,
		ctaX, ctaY, ctaW, ctaH, ctaH/2, t.CTABG,
		w/2, ctaY+ctaH/2, t.CTAFG, t.CTA)
	b.WriteString(`
</svg>`)
	return []byte(b.String())
}

// standardSizes is the list of IAB sizes we generate themed SVGs for.
// Every (theme, size) combination becomes a creative asset at boot,
// uploaded to the s3.bucket as themes/{slug}-{w}x{h}.svg. The seed
// inserter picks the right entry based on each creative's declared
// width × height in the YAML.
var standardSizes = []struct{ W, H int }{
	{300, 250}, // MPU
	{728, 90},  // Leaderboard
	{300, 600}, // Half-page
	{320, 50},  // Mobile banner
	{160, 600}, // Wide skyscraper
	{970, 250},  // Billboard
	{336, 280},  // Large rectangle
	{1200, 627}, // Native main image (1.91:1)
}
