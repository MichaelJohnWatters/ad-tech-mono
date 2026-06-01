package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

var sspSchema = []config.SchemaEntry{
	{Key: "ssp.exchange_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8081", Description: "Exchange URL for bid requests", Service: constants.ServiceSSP, Since: "v1.0"},
	{Key: "ssp.adserver_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8085", Description: "Ad Server URL for /v1/ssp/serve to fetch rendered HTML after auction win", Service: constants.ServiceSSP, Since: "v1.2"},
	{Key: "cache.warm.publishers.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "Poll interval for SSP publisher cache", Service: constants.ServiceSSP, Since: "v1.1"},
	{Key: "cache.warm.placements.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "Poll interval for SSP placement cache", Service: constants.ServiceSSP, Since: "v1.1"},
}
