package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// sspSchema is the SSP's owned config keys. Passed to config.Setup at boot;
// the pod writes the full schema (these + platform defaults) into its
// service_registry row.
var sspSchema = []config.SchemaEntry{
	{Key: "ssp.exchange_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8081", Description: "Where the SSP sends bid requests. Points at the exchange; change to redirect SSP traffic to a different exchange instance.", Service: constants.ServiceSSP, Since: "v1.0"},
	{Key: "ssp.adserver_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8085", Description: "Where /v1/ssp/serve fetches rendered creative HTML after an auction win. Points at the ad server.", Service: constants.ServiceSSP, Since: "v1.2"},
	{Key: "cache.warm.publishers.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory publisher cache refreshes from Postgres. Controls how quickly newly enabled publishers start receiving bid requests.", Service: constants.ServiceSSP, Since: "v1.1"},
	{Key: "cache.warm.placements.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory placement cache refreshes from Postgres. Affects how quickly floor-price and format edits take effect.", Service: constants.ServiceSSP, Since: "v1.1"},
}
