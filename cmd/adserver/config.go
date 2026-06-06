package main

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// adserverSchema is the ad server's owned config keys. Passed to
// config.Setup at boot; the pod writes the full schema (these + platform
// defaults) into its service_registry row.
var adserverSchema = []config.SchemaEntry{
	{Key: "adserver.tracker_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8083", Description: "Base URL templated into impression / click / viewability pixel hrefs in served ad HTML. MUST be browser-reachable — a cluster-internal DNS name like 'tracker:8083' will not resolve from end-user browsers. In pod mode point at the gateway's /v1/t/* reverse proxy (same hostname clients reach the gateway on).", Service: constants.ServiceAdServer, Since: "v1.0"},
	{Key: "adserver.bandit_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Enable Thompson-sampling creative rotation. Off = creatives picked by simple weighted rotation; on = explores under-served variants and converges on winners.", Service: constants.ServiceAdServer, Since: "v1.0"},
	{Key: "adserver.default_creative_ttl", Type: "duration", Tier: config.TierLive, Default: "5m", Description: "How long the creative-metadata cache trusts a row before reloading from Postgres. Lower = quicker creative approval propagation, higher = less DB load.", Service: constants.ServiceAdServer, Since: "v1.0"},
	{Key: "adserver.freq_cap_per_user_per_campaign", Type: "int", Tier: config.TierLive, Default: "5", Description: "Maximum impressions of any single campaign shown to one user inside the cap window. Hard ceiling; per-campaign overrides can only go lower.", Service: constants.ServiceAdServer, Since: "v1.1"},
	{Key: "adserver.freq_cap_window", Type: "duration", Tier: config.TierLive, Default: "24h", Description: "Sliding window for the per-user-per-campaign frequency cap counter. Counter resets when the window expires; longer window = stricter cap.", Service: constants.ServiceAdServer, Since: "v1.1"},
	{Key: "cache.warm.creatives.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory creative-metadata cache refreshes from Postgres. Affects how quickly approved/rejected creatives start/stop serving.", Service: constants.ServiceAdServer, Since: "v1.1"},
}

// Knobs is the ad server's typed config accessor. See cmd/dsp/config.go
// for the pattern explanation.
type Knobs struct {
	cfg *config.Config

	// Live (consumed inside FreqCap / CreativeResolver constructors).
	FreqCapLimit  *config.LiveInt
	FreqCapWindow *config.LiveDuration
	CreativeTTL   *config.LiveDuration
}

func NewKnobs(sc *config.ServiceConfig) *Knobs {
	return &Knobs{
		cfg:           sc.Cfg,
		FreqCapLimit:  config.NewLiveInt(sc.Manager, sc.Cfg, "adserver.freq_cap_per_user_per_campaign", 5),
		FreqCapWindow: config.NewLiveDuration(sc.Manager, sc.Cfg, "adserver.freq_cap_window", 24*time.Hour),
		CreativeTTL:   config.NewLiveDuration(sc.Manager, sc.Cfg, "adserver.default_creative_ttl", 5*time.Minute),
	}
}

// BanditEnabled toggles Thompson-sampling creative rotation. TierLive —
// read per serve request so flipping the flag takes effect immediately.
func (k *Knobs) BanditEnabled() bool { return k.cfg.GetBool("adserver.bandit_enabled", true) }
