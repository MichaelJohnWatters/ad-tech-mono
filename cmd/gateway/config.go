package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

var gatewaySchema = []config.SchemaEntry{
	{Key: "gateway.jwt_signing_key", Type: "string", Tier: config.TierSecret, Default: "", Description: "JWT signing key (empty = dev mode, no auth)", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.dsp_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8082", Description: "DSP service URL", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.ssp_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8084", Description: "SSP service URL", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.adserver_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8085", Description: "Ad Server service URL", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.reporting_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8086", Description: "Reporting service URL", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.exchange_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8081", Description: "Exchange service URL", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.tracker_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8083", Description: "Tracker service URL", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.jaeger_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:16686", Description: "Jaeger query service URL (proxied for browser CORS)", Service: constants.ServiceGateway, Since: "v1.0"},
	{Key: "gateway.config_poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often to poll config from Postgres", Service: constants.ServiceGateway, Since: "v1.0"},
}
