package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// gatewaySchema is the gateway's owned config keys. Passed to config.Setup
// at boot; the pod writes the full schema (these + platform defaults) into
// its service_registry row. The gateway hosts the config-manager UI but
// otherwise owns very few knobs — most behaviour is delegated to the
// services it proxies.
var gatewaySchema = []config.SchemaEntry{
	{Key: "gateway.jwt_signing_key", Type: "string", Tier: config.TierSecret, Default: "", Description: "Secret used to sign and verify JWTs. Empty value = dev mode (auth bypassed). Set via K8s Secret in non-dev environments.", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.dsp_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8082", Description: "Internal DSP service URL the gateway proxies to for /v1/api/campaigns/*.", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.ssp_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8084", Description: "Internal SSP service URL the gateway proxies to for /v1/api/placements/*.", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.adserver_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8085", Description: "Internal ad server URL the gateway proxies to for /v1/api/creatives/*.", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.reporting_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8086", Description: "Internal reporting service URL the gateway proxies to for /v1/api/reports/* and /v1/api/billing/*.", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.exchange_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8081", Description: "Internal exchange URL the gateway proxies to for the OpenRTB try-it-out endpoints.", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.tracker_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8083", Description: "Internal tracker URL the gateway proxies to for the developer pixel-trigger tools.", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.jaeger_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:16686", Description: "Jaeger query API URL proxied for the browser. Needed because Jaeger v1.58 doesn't set CORS headers on its query endpoint.", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.config_poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the gateway polls Postgres for live-config changes. Same semantics as config.poll_interval but lets the gateway tune independently of the platform default.", Service: constants.ServiceGateway, Since: "v1.0"},
}
