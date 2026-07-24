// cmd/demoadv is a standalone DEMO ADVERTISER website ("Ford") — deliberately
// OUTSIDE the ad-tech cluster. It embeds the platform's real advertiser pixels:
//   - the RETARGETING pixel (/v1/t/rt) on page views → the visitor is added to a
//     retargeting audience (consent-gated), targetable in the next auction;
//   - the CONVERSION pixel (/v1/t/conv) on "purchase" → a billed conversion event.
//
// It's the advertiser-side counterpart to cmd/demosite (publisher) and
// cmd/extbidder (DSP): the third external origin. It talks to the platform only
// via the public tracker URL (cross-origin) — a real advertiser is never inside
// your cluster.
//
//	Run locally:  go run ./cmd/demoadv        (then open http://localhost:9200)
//	Config (env): DEMOADV_PORT, DEMOADV_TRACKER_URL, DEMOADV_ACCOUNT_ID,
//	              DEMOADV_BRAND.
package main

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"os"
)

//go:embed templates/*.html
var templatesFS embed.FS

type siteConfig struct {
	TrackerURL string // public base of the tracker (/v1/t/*), browser-reachable
	AccountID  string // the advertiser account id (the `aid` pixel param)
	Brand      string
}

type page struct {
	Cfg    siteConfig
	Active string
	Title  string
	Tag    string // retargeting tag for this page (site_visit rule key)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := siteConfig{
		// Default = the tracker's localhost-exposed port (zero-setup browser use).
		// For the real public path use https://tracker.<domain> (needs /etc/hosts).
		TrackerURL: env("DEMOADV_TRACKER_URL", "http://localhost:8083"),
		// The advertiser account the retargeting audience is scoped to. Use a real
		// seeded advertiser account id (portal → or the seed's advertiser account);
		// the placeholder still fires pixels but won't match a real retargeting rule.
		AccountID: env("DEMOADV_ACCOUNT_ID", "demo-advertiser"),
		Brand:     env("DEMOADV_BRAND", "Ford"),
	}
	port := env("DEMOADV_PORT", "9200")

	tmpl := template.Must(template.ParseFS(templatesFS, "templates/*.html"))
	render := func(name, title, active, tag string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" && name == "home.html" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := tmpl.ExecuteTemplate(w, name, page{Cfg: cfg, Active: active, Title: title, Tag: tag}); err != nil {
				log.Printf("render %s: %v", name, err)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", render("home.html", cfg.Brand, "home", "home"))
	mux.HandleFunc("/models/f150", render("product.html", "F-150 — "+cfg.Brand, "f150", "f150-interest"))
	mux.HandleFunc("/checkout", render("checkout.html", "Checkout — "+cfg.Brand, "checkout", "checkout"))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	log.Printf("demoadv (external advertiser %q) on :%s → tracker %s  account=%s",
		cfg.Brand, port, cfg.TrackerURL, cfg.AccountID)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
