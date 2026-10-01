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
	LandingURLBase:   config.RawString("seed.landing_url_base", "http://localhost:8080/dev/landing"),
	// ShopURLBase is the browser-reachable base of the demo advertiser shop
	// (cmd/demoadv). Product-catalog rows' product_url (the Dynamic Product Ad
	// click target) become "{base}/models/{sku}". Default = the host-run shop
	// (:9200); set seed.shop_url_base / SEED_SHOP_URL_BASE to the in-cluster
	// ingress (https://shop.<domain>) so the DPA chase links back to the shop.
	ShopURLBase: config.RawString("seed.shop_url_base", "http://localhost:9200"),
}
