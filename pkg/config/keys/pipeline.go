package keys

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// Pipeline holds the data-pipeline service's config keys. Raw handles for
// now: cmd/pipeline passes a nil schema to config.Setup, so these keys have
// never been registered — promoting them to a KeySet (and wiring Setup)
// makes them visible in the config-manager UI.
var Pipeline = struct {
	Port                  config.StringKey
	NATSURL               config.StringKey
	DatalakeEnabled       config.BoolKey
	DatalakeBucket        config.StringKey
	DatalakeBatchSize     config.IntKey
	DatalakeFlushInterval config.DurationKey
	OnboardingEnabled     config.BoolKey
	OnboardingBucket      config.StringKey
	OnboardingPollEvery   config.DurationKey
}{
	Port:                  config.RawString("pipeline.port", routes.PortPipeline),
	NATSURL:               config.RawString("pipeline.nats_url", routes.DefaultNATSURL),
	DatalakeEnabled:       config.RawBool("pipeline.datalake_enabled", true),
	DatalakeBucket:        config.RawString("pipeline.datalake_bucket", "adtech-datalake"),
	DatalakeBatchSize:     config.RawInt("pipeline.datalake_batch_size", 500),
	DatalakeFlushInterval: config.RawDuration("pipeline.datalake_flush_interval", 15*time.Second),
	// Third-party audience drop-zone: providers land CSV files in
	// {provider}/incoming/ of the onboarding bucket; the poller validates,
	// normalizes, writes memberships + profile_signals, and quarantines
	// rejects to {provider}/rejected/. Poll-based — Minio S3 events not assumed.
	OnboardingEnabled:   config.RawBool("pipeline.onboarding_enabled", true),
	OnboardingBucket:    config.RawString("pipeline.onboarding_bucket", "adtech-onboarding"),
	OnboardingPollEvery: config.RawDuration("pipeline.onboarding_poll_interval", 30*time.Second),
}
