package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

var reportingSchema = []config.SchemaEntry{
	{Key: "reporting.nats_url", Type: "string", Tier: config.TierStatic, Default: "nats://localhost:4222", Description: "NATS JetStream URL", Service: constants.ServiceReporting, Since: "v1.0"},
	{Key: "reporting.rollup_enabled", Type: "bool", Tier: config.TierLive, Default: "false", Description: "Enable automatic rollup execution", Service: constants.ServiceReporting, Since: "v1.0"},
	{Key: "reporting.billing_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Enable billing on event ingestion", Service: constants.ServiceReporting, Since: "v1.0"},
	{Key: "cache.warm.billing_rates.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "300s", Description: "Poll interval for billing account rate cache", Service: constants.ServiceReporting, Since: "v1.1"},
}
