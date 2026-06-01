package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

var adserverSchema = []config.SchemaEntry{
	{Key: "adserver.tracker_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8083", Description: "Tracker URL for pixel generation", Service: constants.ServiceAdServer, Since: "v1.0"},
	{Key: "adserver.bandit_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Enable Thompson Sampling creative rotation", Service: constants.ServiceAdServer, Since: "v1.0"},
	{Key: "adserver.default_creative_ttl", Type: "duration", Tier: config.TierLive, Default: "5m", Description: "Creative metadata cache TTL", Service: constants.ServiceAdServer, Since: "v1.0"},
	{Key: "adserver.freq_cap_per_user_per_campaign", Type: "int", Tier: config.TierLive, Default: "5", Description: "Default frequency cap (impressions) per user per campaign", Service: constants.ServiceAdServer, Since: "v1.1"},
	{Key: "adserver.freq_cap_window", Type: "duration", Tier: config.TierLive, Default: "24h", Description: "Sliding window for frequency cap counters", Service: constants.ServiceAdServer, Since: "v1.1"},
	{Key: "cache.warm.creatives.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "Poll interval for ad server creative metadata cache", Service: constants.ServiceAdServer, Since: "v1.1"},
}
