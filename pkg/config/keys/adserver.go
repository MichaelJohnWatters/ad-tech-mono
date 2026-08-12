package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var adServerSet = config.NewKeySet(constants.ServiceAdServer)

// AdServerSchema is the AdServer schema — passed to config.Setup at boot.
func AdServerSchema() []config.SchemaEntry { return adServerSet.Entries() }

// AdServer holds the AdServer config keys.
var AdServer = struct {
	TrackerURL                config.StringKey
	BanditEnabled             config.BoolKey
	BanditWarmstart           config.BoolKey
	ReportingURL              config.StringKey
	DefaultCreativeTTL        config.DurationKey
	FreqCapPerUserPerCampaign config.IntKey
	FreqCapWindow             config.DurationKey
	WarmCreativesPollInterval config.DurationKey
	URLTTL                    config.DurationKey
	WarmFreqCapsPollInterval  config.DurationKey
	RateLimitRPS              config.IntKey
	RateLimitBurst            config.IntKey
	RateLimitTrustedHops      config.IntKey
	RateLimitAllowlist        config.StringKey
	RateLimitDistributed      config.BoolKey
	ARASourceRegistration     config.BoolKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL      config.StringKey
	Port     config.StringKey
	GRPCPort config.StringKey

	NATSURL config.StringKey
}{
	TrackerURL:                adServerSet.String("adserver.tracker_url", "http://localhost:8083", config.TierStatic, "Base URL templated into impression / click / viewability pixel hrefs in served ad HTML. MUST be browser-reachable — a cluster-internal DNS name like 'tracker:8083' will not resolve from end-user browsers. In pod mode point at the gateway's /v1/t/* reverse proxy (same hostname clients reach the gateway on).", config.Since("v1.0")),
	BanditEnabled:             adServerSet.Bool("adserver.bandit_enabled", "true", config.TierLive, "Enable Thompson-sampling creative rotation. Off = creatives picked by simple weighted rotation; on = explores under-served variants and converges on winners.", config.Since("v1.0")),
	BanditWarmstart:           adServerSet.Bool("adserver.bandit_warmstart", "true", config.TierStatic, "On boot, seed the bandit's arms from reporting's per-creative impressions/clicks (GET adserver.reporting_url/debug/creative/stats) so a restart doesn't reset every creative to a uniform prior (ADR 0003 part C). Async + fail-open — never blocks boot, never touches the serve hot path.", config.Since("v1.3")),
	ReportingURL:              adServerSet.String("adserver.reporting_url", "http://localhost:8086", config.TierStatic, "Base URL of the reporting service, used only for the bandit warm-start aggregate fetch on boot (adserver.bandit_warmstart).", config.Since("v1.3")),
	DefaultCreativeTTL:        adServerSet.Duration("adserver.default_creative_ttl", "5m", config.TierLive, "How long the creative-metadata cache trusts a row before reloading from Postgres. Lower = quicker creative approval propagation, higher = less DB load.", config.Since("v1.0")),
	FreqCapPerUserPerCampaign: adServerSet.Int("adserver.freq_cap_per_user_per_campaign", "5", config.TierLive, "Maximum impressions of any single campaign shown to one user inside the cap window. Hard ceiling; per-campaign overrides can only go lower.", config.Since("v1.1")),
	FreqCapWindow:             adServerSet.Duration("adserver.freq_cap_window", "5m", config.TierLive, "Sliding window for the per-user-per-campaign frequency cap counter. Counter resets when the window expires; longer window = stricter cap. Default 5m is demo-friendly; production deployments raise this via live config (typical real-world values 1h–24h).", config.Since("v1.1")),
	WarmCreativesPollInterval: adServerSet.Duration("cache.warm.creatives.poll_interval", "30s", config.TierStatic, "How often the in-memory creative-metadata cache refreshes from Postgres. Affects how quickly approved/rejected creatives start/stop serving.", config.Since("v1.1")),
	URLTTL:                    adServerSet.Duration("adserver.url_ttl", "1h", config.TierLive, "Freshness window baked into signed tracker pixel URLs as exp=<unix-ts>. Tracker rejects requests where now > exp. Bounds replay: an attacker who captures a click URL can't fire it after this window expires. Default 1h covers typical session length; 0 disables expiry (forever-valid URLs).", config.Since("v1.2")),
	WarmFreqCapsPollInterval:  adServerSet.Duration("cache.warm.freq_caps.poll_interval", "30s", config.TierLive, "Ad-server per-campaign frequency-cap warm-cache refresh. Campaign PATCH invalidates via the campaigns subject; this bounds staleness otherwise.", config.Since("v1.3")),
	RateLimitRPS:              adServerSet.Int("adserver.ratelimit_rps", "100", config.TierLive, "Per-client-IP HTTP request rate limit (requests/second) on the public ad-serving endpoints. ON by default (100/s per IP, burst 200) — a generous floor against a single abusive IP that won't hurt CGNAT'd real users; the CDN/WAF is the real volumetric shield. 0 = disabled. Buckets are per-pod (× replicas). Allowlisted IPs (ratelimit_allowlist — all private ranges by default, so load tests / the simulator bypass) + infra paths + CORS preflight are never limited.", config.Since("v1.16")),
	RateLimitBurst:            adServerSet.Int("adserver.ratelimit_burst", "200", config.TierLive, "Token-bucket burst for adserver.ratelimit_rps — the max requests allowed in an instantaneous spike before the per-second rate applies. 0 = default to the rps value. Only meaningful when ratelimit_rps > 0.", config.Since("v1.16")),
	RateLimitTrustedHops:      adServerSet.Int("adserver.ratelimit_trusted_proxy_hops", "0", config.TierLive, "Number of trusted reverse proxies in front of the ad server (your ingress, plus any CDN). The rate-limit client IP is taken this many entries from the RIGHT of X-Forwarded-For — the entries a trusted proxy appended — so a client cannot evade the limit by forging (prepending) X-Forwarded-For values. 0 (default) = the ingress is the only trusted hop: use the rightmost XFF entry. Set to 1 when a CDN sits in front of the ingress.", config.Since("v1.17")),
	RateLimitAllowlist:        adServerSet.String("adserver.ratelimit_allowlist", DefaultRateLimitAllowlist, config.TierLive, "Comma-separated CIDRs/IPs that BYPASS the ad server rate limit. Defaults to loopback + private/link-local ranges so internal + local traffic is never throttled. Only consulted when adserver.ratelimit_rps > 0 (off by default — ad serving is high-volume and better shielded at the CDN/WAF).", config.Since("v1.17")),
	RateLimitDistributed:      adServerSet.Bool("adserver.ratelimit_distributed", "false", config.TierLive, "Enforce adserver.ratelimit_rps CLUSTER-WIDE via a shared Redis fixed-window counter instead of per-pod in-process buckets. Costs one Redis op per limited request; fail-open if Redis is unreachable. Default false = per-pod. Only meaningful when ratelimit_rps > 0.", config.Since("v2.0")),
	ARASourceRegistration:     adServerSet.Bool("adserver.ara_source_registration", "false", config.TierLive, "Bake a SIGNED Privacy Sandbox ARA attribution-source beacon (ara_source_url) into the served ad response so a supporting browser registers an attribution source via attributionsrc. Consent-gated (only on a consented serve) and needs a creative landing_url for the destination. Pairs with tracker.ara_enabled (the tracker serves the register-source header + report ingest). Reporting-only overlay — never bills. Default off. See docs/attribution-phase4-ara.md.", config.Since("v2.0")),
	URL:                       config.RawString("adserver.url", routes.DefaultAdServerURL),
	Port:                      config.RawString("adserver.port", routes.PortAdServer),
	GRPCPort:                  config.RawString("adserver.grpc_port", routes.PortAdServerGRPC),
	NATSURL:                   config.RawString("adserver.nats_url", routes.DefaultNATSURL),
}
