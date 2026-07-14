package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var gatewaySet = config.NewKeySet(constants.ServiceGateway)

// GatewaySchema is the Gateway schema — passed to config.Setup at boot.
func GatewaySchema() []config.SchemaEntry { return gatewaySet.Entries() }

// Gateway holds the Gateway config keys.
var Gateway = struct {
	JwtSigningKey      config.StringKey
	RequireAuth        config.BoolKey
	DSPURL             config.StringKey
	SSPURL             config.StringKey
	AdserverURL        config.StringKey
	ReportingURL       config.StringKey
	ExchangeURL        config.StringKey
	TrackerURL         config.StringKey
	JaegerURL          config.StringKey
	ConfigPollInterval config.DurationKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL  config.StringKey
	Port config.StringKey

	PublisherAdServerURL config.StringKey
	CreativesStoreURL    config.StringKey
	ServiceAPIKey        config.StringKey
	SSAIURL              config.StringKey
}{
	JwtSigningKey:        gatewaySet.String("gateway.jwt_signing_key", "", config.TierSecret, "Fallback JWT signing key. Prefer an active jwt_signing secret in the secrets store (rotatable); this config key is the legacy/override path. Empty here AND no secret = auth bypassed (dev only — see gateway.require_auth).", config.Since("v1.0")),
	RequireAuth:          gatewaySet.Bool("gateway.require_auth", "false", config.TierStatic, "When true, the gateway refuses to boot unless a JWT signing key is available (from the secrets store or gateway.jwt_signing_key) — i.e. the dev auth-bypass is forbidden. Set true in staging/prod overlays so a missing key fails loud instead of silently granting admin to every request.", config.Since("v1.3")),
	DSPURL:               gatewaySet.String("gateway.dsp_url", "http://localhost:8082", config.TierStatic, "Internal DSP service URL the gateway proxies to for /v1/api/campaigns/*.", config.Since("v1.0")),
	SSPURL:               gatewaySet.String("gateway.ssp_url", "http://localhost:8084", config.TierStatic, "Internal SSP service URL the gateway proxies to for /v1/api/placements/*.", config.Since("v1.0")),
	AdserverURL:          gatewaySet.String("gateway.adserver_url", "http://localhost:8085", config.TierStatic, "Internal ad server URL the gateway proxies to for /v1/api/creatives/*.", config.Since("v1.0")),
	ReportingURL:         gatewaySet.String("gateway.reporting_url", "http://localhost:8086", config.TierStatic, "Internal reporting service URL the gateway proxies to for /v1/api/reports/* and /v1/api/billing/*.", config.Since("v1.0")),
	ExchangeURL:          gatewaySet.String("gateway.exchange_url", "http://localhost:8081", config.TierStatic, "Internal exchange URL the gateway proxies to for the OpenRTB try-it-out endpoints.", config.Since("v1.0")),
	TrackerURL:           gatewaySet.String("gateway.tracker_url", "http://localhost:8083", config.TierStatic, "Internal tracker URL the gateway proxies to for the developer pixel-trigger tools.", config.Since("v1.0")),
	JaegerURL:            gatewaySet.String("gateway.jaeger_url", "http://localhost:16686", config.TierStatic, "Jaeger query API URL proxied for the browser. Needed because Jaeger v1.58 doesn't set CORS headers on its query endpoint.", config.Since("v1.0")),
	ConfigPollInterval:   gatewaySet.Duration("gateway.config_poll_interval", "30s", config.TierStatic, "How often the gateway polls Postgres for live-config changes. Same semantics as config.poll_interval but lets the gateway tune independently of the platform default.", config.Since("v1.0")),
	URL:                  config.RawString("gateway.url", routes.DefaultGatewayURL),
	Port:                 config.RawString("gateway.port", routes.PortGateway),
	PublisherAdServerURL: config.RawString("gateway.publisher_adserver_url", routes.DefaultPublisherAdServerURL),
	CreativesStoreURL:    config.RawString("gateway.creatives_store_url", ""),
	ServiceAPIKey:        config.RawString("gateway.service_api_key", "dev-api-key-do-not-use-in-prod"),
	SSAIURL:              config.RawString("gateway.ssai_url", routes.DefaultSSAIURL),
}
