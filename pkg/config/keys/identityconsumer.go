package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var identityConsumerSet = config.NewKeySet(constants.ServiceIdentityConsumer)

// IdentityConsumerSchema is the IdentityConsumer schema — passed to config.Setup at boot.
func IdentityConsumerSchema() []config.SchemaEntry { return identityConsumerSet.Entries() }

// IdentityConsumer holds the IdentityConsumer config keys.
var IdentityConsumer = struct {
	Port                    config.StringKey
	NATSURL                 config.StringKey
	FlushInterval           config.DurationKey
	SeenCap                 config.IntKey
	ProbabilisticEnabled    config.BoolKey
	ProbabilisticConfidence config.FloatKey
	FingerprintMaxUsers     config.IntKey
	FuzzyUA                 config.BoolKey
	RedisURL                config.StringKey
	FingerprintTTL          config.DurationKey
}{
	Port:                    identityConsumerSet.String("identity_consumer.port", routes.PortIdentityConsumer, config.TierStatic, "HTTP port for /healthz, /readyz, /metrics.", config.Since("v1.4")),
	NATSURL:                 identityConsumerSet.String("identity_consumer.nats_url", routes.DefaultNATSURL, config.TierStatic, "NATS JetStream URL. Without it no observations are consumed and readiness fails.", config.Since("v1.4")),
	FlushInterval:           identityConsumerSet.Duration("identity_consumer.flush_interval", "10s", config.TierStatic, "How often the consumer flushes its batch of newly-seen identity edges to Postgres.", config.Since("v1.4")),
	SeenCap:                 identityConsumerSet.Int("identity_consumer.seen_cap", "100000", config.TierStatic, "Max size of the in-memory dedup / fingerprint set (bounds memory). Resets when exceeded — re-writing an edge is idempotent.", config.Since("v1.4")),
	ProbabilisticEnabled:    identityConsumerSet.Bool("identity_consumer.probabilistic_enabled", "false", config.TierStatic, "Infer PROBABILISTIC identity links: different users seen from the same IP+user-agent are likely the same device, linked at a lower confidence. Conservative (exact IP+UA; shared IPs skipped). Off by default. Keep a single consumer replica so the fingerprint state stays a coherent global view.", config.Since("v1.4")),
	ProbabilisticConfidence: identityConsumerSet.Float("identity_consumer.probabilistic_confidence", "0.5", config.TierStatic, "Confidence assigned to probabilistic (IP+UA) edges. DSPs can require a higher confidence via dsp.identity_min_confidence to exclude these.", config.Since("v1.4")),
	FingerprintMaxUsers:     identityConsumerSet.Int("identity_consumer.fingerprint_max_users", "5", config.TierStatic, "A fingerprint (IP+UA) seen with more than this many distinct ids is treated as a shared IP and NOT linked — bounds false links across NAT/corporate/carrier IPs.", config.Since("v1.4")),
	FuzzyUA:                 identityConsumerSet.Bool("identity_consumer.fuzzy_ua", "false", config.TierStatic, "Normalise the user-agent (strip version numbers) before fingerprinting, so minor-version churn (Chrome/120 vs /121) doesn't split a device. Off = exact IP+UA (most conservative).", config.Since("v1.4")),
	RedisURL:                identityConsumerSet.String("identity_consumer.redis_url", "", config.TierStatic, "Redis address (host:port) for the probabilistic fingerprint buckets. Set this to run more than one consumer replica — the buckets stay coherent in Redis. Empty = in-memory buckets (requires a single replica).", config.Since("v1.4")),
	FingerprintTTL:          identityConsumerSet.Duration("identity_consumer.fingerprint_ttl", "1h", config.TierStatic, "TTL on a Redis fingerprint bucket — how long an IP+UA is remembered for probabilistic linking. Only used when identity_consumer.redis_url is set.", config.Since("v1.4")),
}
