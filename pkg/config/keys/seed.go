package keys

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"

// Seed holds cmd/seed's asset-URL bases. Raw: seed is a one-shot job, its
// keys were never registered. CreativesURLBase's real default is computed
// from routes at the call site, so the handle default is a sentinel "".
var Seed = struct {
	CreativesURLBase config.StringKey
	LandingURLBase   config.StringKey
	ShopURLBase      config.StringKey
}{
	CreativesURLBase: config.RawString("seed.creatives_url_base", ""),
	// LandingURLBase is baked into each creative's click-through redirect at seed
	// time (NOT host-rewritten at serve time like the beacon URLs), so it must be
	// browser-reachable as-is. Default = the public gateway ingress (serves
	// /dev/landing/{slug}) so click-throughs work in a browser with NO
	// `make demo-forward` bridge. localhost:8080 only resolved behind that bridge.
	// Staging/prod set seed.landing_url_base / SEED_LANDING_URL_BASE to their gateway.
	LandingURLBase: config.RawString("seed.landing_url_base", "https://gateway.adtech.local/dev/landing"),
	// ShopURLBase is the browser-reachable base of the demo advertiser shop.
	// Product-catalog rows' product_url (the Dynamic Product Ad click target)
	// become "{base}/models/{sku}" — baked at seed time, so browser-reachable
	// as-is. Default = the in-cluster shop ingress so the DPA chase links back
	// to the shop with no bridge. Host-run shop (:9200) or staging/prod: set
	// seed.shop_url_base / SEED_SHOP_URL_BASE.
	ShopURLBase: config.RawString("seed.shop_url_base", "https://shop.adtech.local"),
}
