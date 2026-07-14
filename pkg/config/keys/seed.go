package keys

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"

// Seed holds cmd/seed's asset-URL bases. Raw: seed is a one-shot job, its
// keys were never registered. CreativesURLBase's real default is computed
// from routes at the call site, so the handle default is a sentinel "".
var Seed = struct {
	CreativesURLBase config.StringKey
	LandingURLBase   config.StringKey
}{
	CreativesURLBase: config.RawString("seed.creatives_url_base", ""),
	LandingURLBase:   config.RawString("seed.landing_url_base", "http://localhost:8080/dev/landing"),
}
