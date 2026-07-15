package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// ProfileBuilder holds cmd/profile-builder's keys. Raw: one-shot CronJob
// binary, registers no schema (nil Setup) — same posture as dayboundary and
// the privacy jobs.
var ProfileBuilder = struct {
	NATSURL config.StringKey
	// DatalakeBucket must match the pipeline's (the builder reads
	// behaviour_signals/profile_signals from, and writes identity_clusters
	// to, the same lake).
	DatalakeBucket config.StringKey
	// MinConfidence gates which identity edges link during clustering —
	// same dial as dsp.identity_min_confidence on the read-time resolver.
	MinConfidence config.FloatKey
	// MaxClusterSize drops clusters above this as linking pathology (a
	// shared device / polluted probabilistic edges would smear one person's
	// segments over strangers).
	MaxClusterSize config.IntKey
}{
	NATSURL:        config.RawString("profile_builder.nats_url", routes.DefaultNATSURL),
	DatalakeBucket: config.RawString("profile_builder.datalake_bucket", "adtech-datalake"),
	MinConfidence:  config.RawFloat("profile_builder.min_confidence", 0.5),
	MaxClusterSize: config.RawInt("profile_builder.max_cluster_size", 100),
}
