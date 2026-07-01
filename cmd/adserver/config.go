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
	{Key: "adserver.bandit_warmstart", Type: "bool", Tier: config.TierStatic, Default: "true", Description: "On boot, seed the bandit's arms from reporting's per-creative impressions/clicks (GET adserver.reporting_url/debug/creative/stats) so a restart doesn't reset every creative to a uniform prior (ADR 0003 part C). Async + fail-open — never blocks boot, never touches the serve hot path.", Service: constants.ServiceAdServer, Since: "v1.3"},
	{Key: "adserver.reporting_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8086", Description: "Base URL of the reporting service, used only for the bandit warm-start aggregate fetch on boot (adserver.bandit_warmstart).", Service: constants.ServiceAdServer, Since: "v1.3"},
	{Key: "adserver.default_creative_ttl", Type: "duration", Tier: config.TierLive, Default: "5m", Description: "How long the creative-metadata cache trusts a row before reloading from Postgres. Lower = quicker creative approval propagation, higher = less DB load.", Service: constants.ServiceAdServer, Since: "v1.0"},
	{Key: "adserver.freq_cap_per_user_per_campaign", Type: "int", Tier: config.TierLive, Default: "5", Description: "Maximum impressions of any single campaign shown to one user inside the cap window. Hard ceiling; per-campaign overrides can only go lower.", Service: constants.ServiceAdServer, Since: "v1.1"},
	{Key: "adserver.freq_cap_window", Type: "duration", Tier: config.TierLive, Default: "5m", Description: "Sliding window for the per-user-per-campaign frequency cap counter. Counter resets when the window expires; longer window = stricter cap. Default 5m is demo-friendly; production deployments raise this via live config (typical real-world values 1h–24h).", Service: constants.ServiceAdServer, Since: "v1.1"},
	{Key: "cache.warm.creatives.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory creative-metadata cache refreshes from Postgres. Affects how quickly approved/rejected creatives start/stop serving.", Service: constants.ServiceAdServer, Since: "v1.1"},
	{Key: "adserver.url_ttl", Type: "duration", Tier: config.TierLive, Default: "1h", Description: "Freshness window baked into signed tracker pixel URLs as exp=<unix-ts>. Tracker rejects requests where now > exp. Bounds replay: an attacker who captures a click URL can't fire it after this window expires. Default 1h covers typical session length; 0 disables expiry (forever-valid URLs).", Service: constants.ServiceAdServer, Since: "v1.2"},
}

// Knobs is the ad server's typed config accessor. See cmd/dsp/config.go
// for the pattern explanation.
type Knobs struct {
	cfg *config.Config

	// Live (consumed inside FreqCap / CreativeResolver constructors).
	FreqCapLimit  *config.LiveInt
	FreqCapWindow *config.LiveDuration
	CreativeTTL   *config.LiveDuration
	URLTTL        *config.LiveDuration
}

func NewKnobs(sc *config.ServiceConfig) *Knobs {
	return &Knobs{
		cfg:           sc.Cfg,
		FreqCapLimit:  config.NewLiveInt(sc.Manager, sc.Cfg, "adserver.freq_cap_per_user_per_campaign", 5),
		FreqCapWindow: config.NewLiveDuration(sc.Manager, sc.Cfg, "adserver.freq_cap_window", 5*time.Minute),
		CreativeTTL:   config.NewLiveDuration(sc.Manager, sc.Cfg, "adserver.default_creative_ttl", 5*time.Minute),
		URLTTL:        config.NewLiveDuration(sc.Manager, sc.Cfg, "adserver.url_ttl", time.Hour),
	}
}

// BanditEnabled toggles Thompson-sampling creative rotation. TierLive —
// read per serve request so flipping the flag takes effect immediately.
func (k *Knobs) BanditEnabled() bool { return k.cfg.GetBool("adserver.bandit_enabled", true) }
