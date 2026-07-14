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
	RateLimitPerIP              config.IntKey
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
	RateLimitPerIP:              trackerSet.Int("tracker.rate_limit_per_ip", "60", config.TierLive, "Maximum requests per IP per minute. Hitting the limit returns 429 and drops the event. Tune up for legitimate high-traffic publishers.", config.Since("v1.0")),
	DedupTTL:                    trackerSet.Duration("tracker.dedup_ttl", "24h", config.TierLive, "How long the Redis dedup key for an event stays alive. Duplicates within this window are silently dropped.", config.Since("v1.1")),
	DedupEnabled:                trackerSet.Bool("tracker.dedup_enabled", "true", config.TierLive, "Use Redis SetNX to deduplicate identical events (same event type + trace_id). Disable during incident replays where duplicates are expected.", config.Since("v1.1")),
	ExpValidation:               trackerSet.Bool("tracker.exp_validation", "true", config.TierLive, "If true, reject requests whose exp=<unix-ts> param is in the past with 410 Gone. URLs without exp pass unchanged (legacy / unsigned dev calls). Disable for incident replay where stale URLs need to fire.", config.Since("v1.2")),
	WarmFraudRulesPollInterval:  trackerSet.Duration("cache.warm.fraud_rules.poll_interval", "60s", config.TierStatic, "How often the in-memory fraud-rule cache refreshes. Affects how quickly newly added IP/UA blocklist entries start taking effect.", config.Since("v1.1")),
	WarmSigningKeysPollInterval: trackerSet.Duration("cache.warm.signing_keys.poll_interval", "60s", config.TierStatic, "How often the in-memory HMAC signing-key cache refreshes. Drives how quickly a rotated key becomes usable for signature validation.", config.Since("v1.1")),
	URL:                         config.RawString("tracker.url", routes.DefaultTrackerURL),
	Port:                        config.RawString("tracker.port", routes.PortTracker),
}
