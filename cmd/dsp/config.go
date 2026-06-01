package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// dspSchema is the DSP service's owned config keys. Published to the
// config_schema table at boot via config.PublishSchemaWithURL so the
// gateway's config-manager UI sees them without importing this package.
// Adding a new DSP knob = add an entry here.
//
// Platform-shared keys (database.*, redis.*, otel.*, etc.) live in
// pkg/config.defaultSchema() — registered once for every service via the
// init() in pkg/config, not duplicated here.
var dspSchema = []config.SchemaEntry{
	{Key: "dsp.profile", Type: "string", Tier: config.TierStatic, Default: "internal", Description: "DSP name used to look up this pod's row in the dsps table (internal, competitor1, competitor2)", Service: constants.ServiceDSP, Since: "v1.0"},
	{Key: "dsp.daily_budget_default", Type: "float", Tier: config.TierLive, Default: "1000", Description: "Default daily budget for new campaigns", Service: constants.ServiceDSP, Since: "v1.0"},
	{Key: "dsp.max_bid_modifier", Type: "float", Tier: config.TierLive, Default: "200", Description: "Max bid modifier percentage (safety rail)", Service: constants.ServiceDSP, Since: "v1.0"},
	{Key: "dsp.noise_pct", Type: "float", Tier: config.TierLive, Default: "0", Description: "Bid noise % override (e.g. 30 = ±30%). Defaults to the dsps row's noise_pct.", Service: constants.ServiceDSP, Since: "v1.1"},
	{Key: "dsp.no_bid_rate", Type: "float", Tier: config.TierLive, Default: "0", Description: "Probability (0-1) of a random no-bid. Defaults to the dsps row's no_bid_rate.", Service: constants.ServiceDSP, Since: "v1.1"},
	{Key: "dsp.budget_reset_interval", Type: "duration", Tier: config.TierLive, Default: "24h", Description: "TTL for daily budget keys in Redis", Service: constants.ServiceDSP, Since: "v1.1"},
	{Key: "cache.warm.campaigns.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "Poll interval for DSP campaign cache", Service: constants.ServiceDSP, Since: "v1.1"},
}
