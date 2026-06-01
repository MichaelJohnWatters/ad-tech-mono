package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// exchangeSchema is the exchange service's owned config keys. Published to
// the config_schema table at boot. Adding a new auction-side knob = add an
// entry here, no shared-package edit needed.
var exchangeSchema = []config.SchemaEntry{
	{Key: "exchange.channel", Type: "string", Tier: config.TierLive, Default: "all", Description: "Channel filter (all, display, video, etc)", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.bid_timeout", Type: "duration", Tier: config.TierLive, Default: "500ms", Description: "Max time to wait for DSP bids. 100ms is the OpenRTB industry default for prod; locally each DSP is a single Go process so 500ms is more forgiving under concurrent drain/load. Tunable via the config manager.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.dsp_endpoints", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8082,http://localhost:8089,http://localhost:8090", Description: "Comma-separated DSP URLs", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.nats_url", Type: "string", Tier: config.TierStatic, Default: "nats://localhost:4222", Description: "NATS JetStream URL", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.max_retries", Type: "int", Tier: config.TierStatic, Default: "30", Description: "Max port bind retries on startup", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.win_loss_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Send win/loss notifications to DSPs", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "cache.warm.deals.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "Poll interval for exchange deal cache", Service: constants.ServiceExchange, Since: "v1.1"},
}
