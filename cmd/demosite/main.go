// cmd/demosite is a standalone DEMO PUBLISHER website — deliberately OUTSIDE the
// ad-tech cluster. It embeds the real adtech.js SDK / VAST tags and requests real
// ads from the platform's PUBLIC ingress (pubad), exactly as a third-party
// publisher would. Running it as a separate origin (host process on :9000, or a
// container in a second cluster) is the point: it exercises the real cross-origin
// path — CORS, TLS, the configurable SDK host, public ingress routing — that an
// in-cluster page would paper over.
//
// It talks to the platform ONLY via configurable public URLs (never in-cluster
// DNS), so "host process now, second cluster later" is a deploy choice, not a
// code change.
//
//	Run locally:  go run ./cmd/demosite   (then open http://localhost:9000)
//	Config (env): DEMOSITE_PORT, DEMOSITE_PUBAD_URL, DEMOSITE_SDK_URL,
//	              DEMOSITE_MEDIA_URL, DEMOSITE_PUBLISHER_ID
package main

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/pages"
)

//go:embed templates/*.html
var templatesFS embed.FS

// siteConfig is injected into every page — all the platform-facing URLs the
// browser uses. Defaults target the local HTTPS ingress (Phase A); override via
// env for staging/prod (the real public domain).
type siteConfig struct {
	PubadURL    string // public base of the publisher-adserver (/v1/pubad/*)
	SDKURL      string // where the browser loads adtech.js from
	MediaURL    string // base for video/audio media + creative assets
	PublisherID string // the seeded demo publisher
	// Branding + which page layouts this instance runs. Driven by DEMOSITE_SITE
	// (a pkg/simulator/pages Site slug): one demosite binary renders any of the
	// "friend's website" properties as its own branded origin. Unset = the
	// default demo property showing every layout.
	SiteName    string
	SiteTagline string
	Kind        string         // pages.Site.Kind — drives nav + which pages this origin shows
	Layouts     []pages.Layout // the layouts this site shows (its "Pages" nav)
	// Per-site visual identity + navigation (cmd/demosite/chrome.go). Theme paints
	// the palette/typography (via ThemeCSS), Nav is the bespoke top bar, Home is the
	// template the "/" route renders. Populated from siteChrome[slug].
	Theme themeSpec
	Nav   []navLink
	Home  string
	// Per-format seeded placement external IDs (idgen-derived server-side).
	DisplayPlacement string
	VideoPlacement   string
	AudioPlacement   string
	NativePlacement  string
	// SSAIURL is the BROWSER-reachable base of the SSAI stitcher. The stitcher
	// has no public ingress of its own; the browser reaches it via the gateway
	// proxy (routes.ProxySSAI = /v1/ssai — cmd/ssai/CLAUDE.md), so this defaults
	// to the gateway.
	SSAIURL string
}

type page struct {
	Cfg     siteConfig
	Active  string // nav highlight
	Title   string
	Layout  pages.Layout   // the multi-slot page being rendered (page.html)
	Layouts []pages.Layout // all layouts (pages_index.html)
	Vid     *videoSpec     // the ViewTube watch page being rendered (viewtube_watch.html)
}

// videoSpec is one ViewTube "video". Breaks is the VMAP break subset the player
// requests (publisher-adserver's /v1/pubad/video/vmap?breaks=…), so the three demo
// videos show pre-roll / pre+mid / pre+mid+post without any new backend state.
type videoSpec struct {
	ID          string
	Title       string
	Channel     string
	Views       string
	Breaks      string // "pre" | "pre,mid" | "pre,mid,post"
	BreaksLabel string
}

// VideoList returns the ViewTube demo videos in display order (for the home grid).
func (page) VideoList() []videoSpec {
	return []videoSpec{viewtubeVideos["1"], viewtubeVideos["2"], viewtubeVideos["3"]}
}

// viewtubeVideos — the three demo videos, each demonstrating one break schedule.
var viewtubeVideos = map[string]videoSpec{
	"1": {ID: "1", Title: "Building a bid request from scratch", Channel: "AdTech Explained", Views: "48K views", Breaks: "pre", BreaksLabel: "Pre-roll only"},
	"2": {ID: "2", Title: "How a first-price auction clears", Channel: "AdTech Explained", Views: "12K views", Breaks: "pre,mid", BreaksLabel: "Pre-roll + mid-roll"},
	"3": {ID: "3", Title: "SSAI vs client-side VAST, explained", Channel: "AdTech Explained", Views: "203K views", Breaks: "pre,mid,post", BreaksLabel: "Pre-roll + mid-roll + post-roll"},
}

// PlacementFor maps a page layout's Slot.Format to THIS site's own seeded
// placement external key. The pkg/simulator/pages Layout carries the shared
// pub-simulator placement keys (so the e2e can replay them), but a branded
// demosite instance must serve — and attribute impressions to — its OWN
// publisher's placements. We therefore override the slot's placement by format
// at render time using the per-format DEMOSITE_*_PLACEMENT env values captured
// in siteConfig, falling back to the layout's own key when unset (the default
// pub-simulator behaviour — backward compatible).
func (c siteConfig) PlacementFor(s pages.Slot) string {
	switch s.Format {
	case pages.Display:
		if c.DisplayPlacement != "" {
			return c.DisplayPlacement
		}
	case pages.Native:
		if c.NativePlacement != "" {
			return c.NativePlacement
		}
	case pages.Video:
		if c.VideoPlacement != "" {
			return c.VideoPlacement
		}
	case pages.Audio:
		if c.AudioPlacement != "" {
			return c.AudioPlacement
		}
	}
	return s.PlacementKey
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := siteConfig{
		// Defaults target the LOCALHOST-exposed ports so `go run ./cmd/demosite` +
		// open http://localhost:9000 works in a browser with zero setup. To exercise
		// the realistic PUBLIC path (TLS ingress + CORS, as a cloud-hosted publisher
		// would), override with the ingress hostnames (needs /etc/hosts → 127.0.0.1):
		//   DEMOSITE_PUBAD_URL=https://pubad.adtech.local \
		//   DEMOSITE_SDK_URL=https://gateway.adtech.local/static/adtech.js \
		//   DEMOSITE_MEDIA_URL=https://gateway.adtech.local  go run ./cmd/demosite
		PubadURL:         env("DEMOSITE_PUBAD_URL", "http://localhost:8088"),
		SDKURL:           env("DEMOSITE_SDK_URL", "http://localhost:8080/static/adtech.js"),
		MediaURL:         env("DEMOSITE_MEDIA_URL", "http://localhost:8080"),
		SSAIURL:          env("DEMOSITE_SSAI_URL", "http://localhost:8080/v1/ssai"),
		PublisherID:      env("DEMOSITE_PUBLISHER_ID", "pub-simulator"),
		DisplayPlacement: env("DEMOSITE_DISPLAY_PLACEMENT", "pl-sim-mpu"),
		VideoPlacement:   env("DEMOSITE_VIDEO_PLACEMENT", "pl-sim-video"),
		AudioPlacement:   env("DEMOSITE_AUDIO_PLACEMENT", "pl-sim-audio"),
		NativePlacement:  env("DEMOSITE_NATIVE_PLACEMENT", "pl-sim-native"),
		SiteName:         "The Demo Times",
		SiteTagline:      "An external publisher · powered by adtech.js",
		Layouts:          pages.All(),
	}
	// DEMOSITE_SITE picks one of the named "friend's website" properties
	// (pkg/simulator/pages) — its branding + its own page set — so the same
	// binary can be run as several distinct branded origins (see `make demosites`).
	slug := env("DEMOSITE_SITE", "")
	if slug != "" {
		site, ok := pages.SiteBySlug(slug)
		if !ok {
			log.Fatalf("DEMOSITE_SITE=%q is not a known site (have: run `make demosites`)", slug)
		}
		cfg.SiteName = site.Name
		cfg.SiteTagline = site.Tagline
		cfg.Kind = site.Kind
		cfg.Layouts = site.Layouts()
	}
	// Per-site visual identity + nav (chrome.go). Unknown/empty slug → defaultChrome
	// (the legacy look), so the plain `go run ./cmd/demosite` is unchanged.
	ch := resolveChrome(slug)
	cfg.Theme, cfg.Nav, cfg.Home = ch.Theme, ch.Nav, ch.Home
	// Explicit masthead override — lets a deployment brand the site exactly (e.g.
	// "DEMO SITE - The Daily Chronicle") independent of the DEMOSITE_SITE slug's
	// pages.Site name, so the property is trivially findable by its "DEMO SITE -"
	// prefix everywhere it surfaces.
	if name := env("DEMOSITE_SITE_NAME", ""); name != "" {
		cfg.SiteName = name
	}
	port := env("DEMOSITE_PORT", "9000")

	tmpl := template.Must(template.ParseFS(templatesFS, "templates/*.html"))

	render := func(name, title, active string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := tmpl.ExecuteTemplate(w, name, page{Cfg: cfg, Active: active, Title: title}); err != nil {
				log.Printf("render %s: %v", name, err)
			}
		}
	}

	mux := http.NewServeMux()
	// The homepage template depends on the property's Kind: an audio-only app
	// (SoundWave) opens on the player, an SSAI/live app (Twitchr) opens on the
	// live stitched stream; everything else keeps the news homepage. The "/"
	// handler is a catch-all in http.ServeMux, so 404 any non-root path here.
	// The home template comes from the site's chrome (bespoke properties). When a
	// site has no bespoke home yet, fall back to the Kind-based default so it still
	// renders exactly as before. The active nav key is whichever nav link points at "/".
	homeTmpl := cfg.Home
	if homeTmpl == "" {
		homeTmpl = "home.html"
		switch cfg.Kind {
		case "audio":
			homeTmpl = "audio.html"
		case "ssai":
			homeTmpl = "ssai.html"
		}
	}
	homeActive := "home"
	for _, l := range cfg.Nav {
		if l.Href == "/" {
			homeActive = l.Key
			break
		}
	}
	homeRender := render(homeTmpl, cfg.SiteName, homeActive)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		homeRender(w, r)
	})
	mux.HandleFunc("/video", render("video.html", "Video — "+cfg.SiteName, "video"))
	mux.HandleFunc("/audio", render("audio.html", "Audio — "+cfg.SiteName, "audio"))
	mux.HandleFunc("/native", render("native.html", "Native — "+cfg.SiteName, "native"))
	// Live/CTV page with server-side ad insertion (Twitchr). The player fetches
	// the STITCHED manifest from the SSAI stitcher (via the gateway proxy) — the
	// ad breaks are spliced server-side, never fetched by the client.
	mux.HandleFunc("/live", render("ssai.html", "Live — "+cfg.SiteName, "live"))

	// ViewTube watch pages: /watch/{1,2,3}. Each renders the same player template
	// but requests a different VMAP break schedule (pre / pre+mid / pre+mid+post),
	// so the three videos demonstrate escalating ad loads.
	mux.HandleFunc("/watch/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/watch/")
		v, ok := viewtubeVideos[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "viewtube_watch.html", page{Cfg: cfg, Active: "w" + id, Title: v.Title + " — " + cfg.SiteName, Vid: &v}); err != nil {
			log.Printf("render viewtube_watch: %v", err)
		}
	})

	// Multi-slot combo pages, defined once in pkg/simulator/pages (the same
	// layouts the e2e replays). /pages lists THIS site's set; /p/{slug} renders one
	// (restricted to this site's layouts so each branded origin shows its own pages).
	inSite := func(slug string) (pages.Layout, bool) {
		for _, l := range cfg.Layouts {
			if l.Slug == slug {
				return l, true
			}
		}
		return pages.Layout{}, false
	}
	mux.HandleFunc("/pages", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "pages_index.html", page{Cfg: cfg, Active: "pages", Title: "Pages — " + cfg.SiteName, Layouts: cfg.Layouts}); err != nil {
			log.Printf("render pages_index: %v", err)
		}
	})
	mux.HandleFunc("/p/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/p/")
		layout, ok := inSite(slug)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "page.html", page{Cfg: cfg, Active: "pages", Title: layout.Title + " — " + cfg.SiteName, Layout: layout}); err != nil {
			log.Printf("render page %s: %v", slug, err)
		}
	})

	// Themed favicon so each demo site has a branded tab icon (and no 404 noise in
	// the trace/dev-tools during a demo recording). An inline SVG tinted with the
	// site's accent + its wordmark initial — one handler covers all six sites.
	faviconInitial := "●"
	for _, r := range cfg.SiteName {
		faviconInitial = string(r)
		break
	}
	favicon := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">`+
		`<rect width="64" height="64" rx="12" fill="%s"/>`+
		`<text x="32" y="46" font-size="40" font-family="Arial,sans-serif" font-weight="bold" `+
		`text-anchor="middle" fill="%s">%s</text></svg>`,
		cfg.Theme.Accent, cfg.Theme.AccentInk, faviconInitial)
	faviconHandler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(favicon))
	}
	mux.HandleFunc("/favicon.ico", faviconHandler)
	mux.HandleFunc("/favicon.svg", faviconHandler)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	log.Printf("demosite (external publisher) on :%s → pubad %s", port, cfg.PubadURL)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
