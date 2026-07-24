package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var trackerSet = config.NewKeySet(constants.ServiceTracker)

// TrackerSchema is the Tracker schema — passed to config.Setup at boot.
func TrackerSchema() []config.SchemaEntry { return trackerSet.Entries() }

// Tracker holds the Tracker config keys.
var Tracker = struct {
	NATSURL                     config.StringKey
	ReportingURL                config.StringKey
	SigningKey                  config.StringKey
	FraudEnabled                config.BoolKey
	SignatureValidation         config.BoolKey
	RateLimitRPS                config.IntKey
	RateLimitBurst              config.IntKey
	RateLimitTrustedHops        config.IntKey
	RateLimitAllowlist          config.StringKey
	DedupTTL                    config.DurationKey
	DedupEnabled                config.BoolKey
	ExpValidation               config.BoolKey
	WarmFraudRulesPollInterval  config.DurationKey
	WarmSigningKeysPollInterval config.DurationKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL  config.StringKey
	Port config.StringKey
}{
	NATSURL:                     trackerSet.String("tracker.nats_url", "nats://localhost:4222", config.TierStatic, "NATS JetStream URL where impression/click/conversion/view events are published.", config.Since("v1.0")),
	ReportingURL:                trackerSet.String("tracker.reporting_url", "http://localhost:8086", config.TierStatic, "HTTP fallback URL for the reporting service. Used if NATS is unavailable so events still reach the analytics store.", config.Since("v1.0")),
	SigningKey:                  trackerSet.String("tracker.signing_key", "adtech-dev-signing-key-change-in-prod", config.TierSecret, "HMAC secret used to sign pixel URLs. Rotating this immediately invalidates every outstanding tracking URL — coordinate with ad server creative regeneration.", config.Since("v1.0")),
	FraudEnabled:                trackerSet.Bool("tracker.fraud_enabled", "true", config.TierLive, "Run real-time fraud checks (bot UA, IP blocklist, rate limit) before recording an event. Disable only for clean-room investigations.", config.Since("v1.0")),
	SignatureValidation:         trackerSet.Bool("tracker.signature_validation", "false", config.TierLive, "If true, requests without a valid HMAC sig param are rejected with 403. If false, signatures are logged as warnings but the event is still recorded — useful while phasing in signing.", config.Since("v1.0")),
	RateLimitRPS:                trackerSet.Int("tracker.ratelimit_rps", "200", config.TierLive, "Per-client-IP rate limit (requests/second) on the tracker pixel endpoints. ON by default (200/s per IP, burst 400 — highest because pixels are the highest-volume path; forgery is separately blocked by tracker.signature_validation, and the CDN/WAF is the volumetric shield). 0 = disabled. Buckets are per-pod (× replicas). Allowlisted IPs (ratelimit_allowlist — all private ranges by default, so load tests / the simulator bypass) + infra paths are never limited. Replaces the old unwired tracker.rate_limit_per_ip.", config.Since("v1.17")),
	RateLimitBurst:              trackerSet.Int("tracker.ratelimit_burst", "400", config.TierLive, "Token-bucket burst for tracker.ratelimit_rps. 0 = default to the rps value. Only meaningful when ratelimit_rps > 0.", config.Since("v1.17")),
	RateLimitTrustedHops:        trackerSet.Int("tracker.ratelimit_trusted_proxy_hops", "0", config.TierLive, "Trusted reverse-proxy count in front of the tracker: the rate-limit client IP is taken this many entries from the RIGHT of X-Forwarded-For so it can't be forged. 0 = single ingress (rightmost). Set 1 behind a CDN.", config.Since("v1.17")),
	RateLimitAllowlist:          trackerSet.String("tracker.ratelimit_allowlist", DefaultRateLimitAllowlist, config.TierLive, "Comma-separated CIDRs/IPs that BYPASS the tracker rate limit. Defaults to loopback + private/link-local ranges so internal + local traffic is never throttled. Only consulted when tracker.ratelimit_rps > 0.", config.Since("v1.17")),
	DedupTTL:                    trackerSet.Duration("tracker.dedup_ttl", "24h", config.TierLive, "How long the Redis dedup key for an event stays alive. Duplicates within this window are silently dropped.", config.Since("v1.1")),
	DedupEnabled:                trackerSet.Bool("tracker.dedup_enabled", "true", config.TierLive, "Use Redis SetNX to deduplicate identical events (same event type + trace_id). Disable during incident replays where duplicates are expected.", config.Since("v1.1")),
	ExpValidation:               trackerSet.Bool("tracker.exp_validation", "true", config.TierLive, "If true, reject requests whose exp=<unix-ts> param is in the past with 410 Gone. URLs without exp pass unchanged (legacy / unsigned dev calls). Disable for incident replay where stale URLs need to fire.", config.Since("v1.2")),
	WarmFraudRulesPollInterval:  trackerSet.Duration("cache.warm.fraud_rules.poll_interval", "60s", config.TierStatic, "How often the in-memory fraud-rule cache refreshes. Affects how quickly newly added IP/UA blocklist entries start taking effect.", config.Since("v1.1")),
	WarmSigningKeysPollInterval: trackerSet.Duration("cache.warm.signing_keys.poll_interval", "60s", config.TierStatic, "How often the in-memory HMAC signing-key cache refreshes. Drives how quickly a rotated key becomes usable for signature validation.", config.Since("v1.1")),
	URL:                         config.RawString("tracker.url", routes.DefaultTrackerURL),
	Port:                        config.RawString("tracker.port", routes.PortTracker),
}
