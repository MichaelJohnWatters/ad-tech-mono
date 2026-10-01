// cmd/demoadv is a standalone DEMO ADVERTISER website ("Ford") — deliberately
// OUTSIDE the ad-tech cluster. It embeds the platform's real advertiser pixels:
//   - the RETARGETING pixel (/v1/t/rt) on page views → the visitor is added to a
//     retargeting audience (consent-gated), targetable in the next auction. This
//     is browser-fired (measurement/audience — not signed);
//   - the CONVERSION (/v1/t/conv) on "purchase" → a billed CPA event. Because it
//     drives billing it is HMAC-signed and fired SERVER-TO-SERVER: the browser
//     POSTs the sale to this advertiser's /convert, whose handler signs the
//     tracker URL with its platform-issued key and fires it. Never browser-fired.
//
// It's the advertiser-side counterpart to cmd/demosite (publisher) and
// cmd/extbidder (DSP): the third external origin. It talks to the platform only
// via the public tracker URL (cross-origin) — a real advertiser is never inside
// your cluster.
//
//	Run locally:  go run ./cmd/demoadv        (then open http://localhost:9200)
//	Config (env): DEMOADV_PORT, DEMOADV_TRACKER_URL, DEMOADV_SDK_URL,
//	              DEMOADV_ACCOUNT_ID, DEMOADV_SIGNING_KEY, DEMOADV_BRAND.
package main

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
)

//go:embed templates/*.html
var templatesFS embed.FS

type siteConfig struct {
	TrackerURL string // public base of the tracker (/v1/t/*), browser-reachable
	// ConvTrackerURL is the base the SERVER-SIDE /convert postback fires to. It
	// defaults to TrackerURL (host/real-external: the advertiser's server reaches
	// the public tracker). But when this shop runs INSIDE the cluster (the
	// demostore deploy), the public ingress hostname (tracker.<domain>) doesn't
	// resolve from a pod — so the deploy sets DEMOADV_TRACKER_INTERNAL_URL to the
	// in-cluster tracker service. The HMAC signs path+params, not host, so the
	// signature is valid regardless of which base the postback uses.
	ConvTrackerURL string
	SDKURL         string // where the browser loads the advertiser tag (adtech-adv.js)
	AccountID      string // the advertiser account id (the `aid`/`advid` param)
	SigningKey     string // the platform-issued HMAC key this advertiser signs its
	// server-to-server conversion postbacks with (a real advertiser is issued its
	// own key; the demo uses the dev key).
	Brand string
	// Product is the hero product's display name ("F-150" by default) so the
	// shop can wear another advertiser's skin — e.g. the themed world's
	// "Premium Dog Food Co" demo runs it as a 12kg dog-food bag — without
	// forking templates. The /models/f150 route + its pixel tag stay stable.
	Product string
	// SKU is the catalog SKU the hero product maps to (Dynamic Product Ads).
	// The product + checkout pages fire it on the retargeting pixel
	// (setProductSKUs) so a dynamic creative renders THIS product. Default
	// matches the seeded dog-food catalog (the chase demo runs as barkbox);
	// a SKU with no catalog row just falls back to the static creative.
	SKU string
}

type page struct {
	Cfg    siteConfig
	Active string
	Title  string
	Tag    string // retargeting tag for this page (site_visit rule key)
	SKU    string // catalog SKU this page's product maps to ("" = no product, e.g. home)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// env2 returns v when non-empty, else def (for form values with a fallback).
func env2(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func main() {
	cfg := siteConfig{
		// Default = the tracker's localhost-exposed port (zero-setup browser use).
		// For the real public path use https://tracker.<domain> (needs /etc/hosts).
		TrackerURL: env("DEMOADV_TRACKER_URL", "http://localhost:8083"),
		// Server-side postback base: defaults to the public TrackerURL; the
		// in-cluster demostore overrides it to the tracker's cluster service.
		ConvTrackerURL: env("DEMOADV_TRACKER_INTERNAL_URL", env("DEMOADV_TRACKER_URL", "http://localhost:8083")),
		// The advertiser tag is served from the platform's static assets (gateway),
		// the same origin adtech.js ships from.
		SDKURL: env("DEMOADV_SDK_URL", "http://localhost:8080/static/adtech-adv.js"),
		// The advertiser account the retargeting audience is scoped to. Use a real
		// seeded advertiser account id (portal → or the seed's advertiser account);
		// the placeholder still fires pixels but won't match a real retargeting rule.
		AccountID: env("DEMOADV_ACCOUNT_ID", "demo-advertiser"),
		// The platform issues each advertiser its OWN signing key for S2S
		// conversion postbacks (G7). A real advertiser generates/rotates it in the
		// portal (Conversions → Conversion signing key, POST /v1/api/conversion-key)
		// and sets it here as DEMOADV_SIGNING_KEY. The demo default is the
		// deterministic dev key the seed mints for this account
		// (adserving.DevConversionKey), resolved just below once AccountID is known,
		// so a demoadv pointed at a seeded advertiser signs correctly under the
		// prod-shaped strict per-advertiser posture with zero setup.
		SigningKey: env("DEMOADV_SIGNING_KEY", ""),
		Brand:      env("DEMOADV_BRAND", "Ford"),
		Product:    env("DEMOADV_PRODUCT", "F-150"),
		SKU:        env("DEMOADV_SKU", "DOG-KIBBLE-12KG"),
	}
	// No explicit key set → use the deterministic dev key the seed minted for
	// this advertiser account, so S2S conversions validate under the strict
	// per-advertiser posture out of the box.
	if cfg.SigningKey == "" {
		cfg.SigningKey = adserving.DevConversionKey(cfg.AccountID)
	}
	port := env("DEMOADV_PORT", "9200")

	tmpl := template.Must(template.ParseFS(templatesFS, "templates/*.html"))
	// render builds a page; sku is the catalog SKU this page's product maps to
	// (empty for the home page, which shows no specific product) — it rides the
	// retargeting pixel so a dynamic creative can render the viewed product.
	render := func(name, title, active, tag, sku string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" && name == "home.html" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := tmpl.ExecuteTemplate(w, name, page{Cfg: cfg, Active: active, Title: title, Tag: tag, SKU: sku}); err != nil {
				log.Printf("render %s: %v", name, err)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", render("home.html", cfg.Brand, "home", "home", ""))
	mux.HandleFunc("/models/f150", render("product.html", "F-150 — "+cfg.Brand, "f150", "f150-interest", cfg.SKU))
	mux.HandleFunc("/checkout", render("checkout.html", "Checkout — "+cfg.Brand, "checkout", "checkout", cfg.SKU))

	// Server-to-server SIGNED conversion postback. A conversion is the platform's
	// CPA BILLING trigger, so /v1/t/conv is HMAC-signed and must NOT be fired
	// unsigned from the browser (billing fraud). The browser only POSTs the
	// conversion facts to THIS advertiser's own server; the server signs the
	// tracker URL with its platform-issued key and fires it S2S. The visitor id
	// for attribution comes from the first-party cookie the adtech-adv.js tag set.
	mux.HandleFunc("/convert", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		convType := env2(r.FormValue("type"), "purchase")
		rev := env2(r.FormValue("rev"), "0")
		uid := ""
		if c, err := r.Cookie("adtechadv_uid"); err == nil {
			uid = c.Value
		}
		// The earning click trace the adtech-adv.js tag captured off the landing
		// URL (?adtech_tid=...) and stored first-party. Returning it as ctid is
		// what lets the platform attribute this conversion to the impression that
		// earned it and settle CPA against the right reservation (deterministic
		// click-through). Absent (no ad click drove this visit) => unattributed.
		ctid := ""
		if c, err := r.Cookie("adtech_ctid"); err == nil {
			ctid = c.Value
		}
		params := url.Values{}
		params.Set("tid", "order-"+strconv.FormatInt(time.Now().UnixNano(), 10))
		params.Set("type", convType)
		params.Set("rev", rev)
		params.Set("cur", "USD")
		params.Set("advid", cfg.AccountID)
		if uid != "" {
			params.Set("uid", uid)
		}
		if ctid != "" {
			params.Set("ctid", ctid)
		}
		// DPA: the purchased SKU (from the checkout page) drives per-product
		// suppression + cross-sell — the chase stops showing the bought product
		// and rotates to its complement.
		if sku := env2(r.FormValue("sku"), cfg.SKU); sku != "" {
			params.Set("skus", sku)
		}
		signed := adserving.SignURL(cfg.ConvTrackerURL+"/v1/t/conv?"+params.Encode(), cfg.SigningKey)
		status := 0
		if resp, err := http.Get(signed); err == nil {
			status = resp.StatusCode
			resp.Body.Close()
		}
		log.Printf("demoadv S2S conversion postback: type=%s rev=%s uid=%s ctid=%s → tracker HTTP %d", convType, rev, uid, ctid, status)
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"ok":true}`))
		} else {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"ok":false}`))
		}
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	log.Printf("demoadv (external advertiser %q) on :%s → tracker %s  account=%s",
		cfg.Brand, port, cfg.TrackerURL, cfg.AccountID)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
