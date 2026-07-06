package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// identityConsumerSchema owns the batching + probabilistic knobs that used to
// live on the SSP — the write (and its state) now lives here, in one consumer.
var identityConsumerSchema = []config.SchemaEntry{
	{Key: "identity_consumer.port", Type: "string", Tier: config.TierStatic, Default: routes.PortIdentityConsumer, Description: "HTTP port for /healthz, /readyz, /metrics.", Service: constants.ServiceIdentityConsumer, Since: "v1.4"},
	{Key: "identity_consumer.nats_url", Type: "string", Tier: config.TierStatic, Default: routes.DefaultNATSURL, Description: "NATS JetStream URL. Without it no observations are consumed and readiness fails.", Service: constants.ServiceIdentityConsumer, Since: "v1.4"},
	{Key: "identity_consumer.flush_interval", Type: "duration", Tier: config.TierStatic, Default: "10s", Description: "How often the consumer flushes its batch of newly-seen identity edges to Postgres.", Service: constants.ServiceIdentityConsumer, Since: "v1.4"},
	{Key: "identity_consumer.seen_cap", Type: "int", Tier: config.TierStatic, Default: "100000", Description: "Max size of the in-memory dedup / fingerprint set (bounds memory). Resets when exceeded — re-writing an edge is idempotent.", Service: constants.ServiceIdentityConsumer, Since: "v1.4"},
	{Key: "identity_consumer.probabilistic_enabled", Type: "bool", Tier: config.TierStatic, Default: "false", Description: "Infer PROBABILISTIC identity links: different users seen from the same IP+user-agent are likely the same device, linked at a lower confidence. Conservative (exact IP+UA; shared IPs skipped). Off by default. Keep a single consumer replica so the fingerprint state stays a coherent global view.", Service: constants.ServiceIdentityConsumer, Since: "v1.4"},
	{Key: "identity_consumer.probabilistic_confidence", Type: "float", Tier: config.TierStatic, Default: "0.5", Description: "Confidence assigned to probabilistic (IP+UA) edges. DSPs can require a higher confidence via dsp.identity_min_confidence to exclude these.", Service: constants.ServiceIdentityConsumer, Since: "v1.4"},
	{Key: "identity_consumer.fingerprint_max_users", Type: "int", Tier: config.TierStatic, Default: "5", Description: "A fingerprint (IP+UA) seen with more than this many distinct ids is treated as a shared IP and NOT linked — bounds false links across NAT/corporate/carrier IPs.", Service: constants.ServiceIdentityConsumer, Since: "v1.4"},
}
