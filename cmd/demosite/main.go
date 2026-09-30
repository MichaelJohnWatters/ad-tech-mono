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
	Layouts     []pages.Layout // the layouts this site shows (its "Pages" nav)
	// Per-format seeded placement external IDs (idgen-derived server-side).
	DisplayPlacement string
	VideoPlacement   string
	AudioPlacement   string
	NativePlacement  string
}

type page struct {
	Cfg     siteConfig
	Active  string // nav highlight
	Title   string
	Layout  pages.Layout   // the multi-slot page being rendered (page.html)
	Layouts []pages.Layout // all layouts (pages_index.html)
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
	if slug := env("DEMOSITE_SITE", ""); slug != "" {
		site, ok := pages.SiteBySlug(slug)
		if !ok {
			log.Fatalf("DEMOSITE_SITE=%q is not a known site (have: run `make demosites`)", slug)
		}
		cfg.SiteName = site.Name
		cfg.SiteTagline = site.Tagline
		cfg.Layouts = site.Layouts()
	}
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
			if r.URL.Path != "/" && name == "home.html" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := tmpl.ExecuteTemplate(w, name, page{Cfg: cfg, Active: active, Title: title}); err != nil {
				log.Printf("render %s: %v", name, err)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", render("home.html", cfg.SiteName, "home"))
	mux.HandleFunc("/video", render("video.html", "Video — "+cfg.SiteName, "video"))
	mux.HandleFunc("/audio", render("audio.html", "Audio — "+cfg.SiteName, "audio"))
	mux.HandleFunc("/native", render("native.html", "Native — "+cfg.SiteName, "native"))

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

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	log.Printf("demosite (external publisher) on :%s → pubad %s", port, cfg.PubadURL)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
