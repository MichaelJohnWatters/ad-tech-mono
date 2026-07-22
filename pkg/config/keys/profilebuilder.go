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

	// ClickHouse* point the phase-2 reads (behavioural rule GROUP BY +
	// reconcile) at the SAME ClickHouse reporting writes behaviour_signals/
	// profile_signals into (ADR 0006). Mirror reporting.clickhouse_*. Empty
	// addr disables the querier → fall back to the lake reads.
	ClickHouseAddr     config.StringKey
	ClickHouseDatabase config.StringKey
	ClickHouseUser     config.StringKey
	ClickHousePassword config.StringKey
}{
	NATSURL:        config.RawString("profile_builder.nats_url", routes.DefaultNATSURL),
	DatalakeBucket: config.RawString("profile_builder.datalake_bucket", "adtech-datalake"),
	MinConfidence:  config.RawFloat("profile_builder.min_confidence", 0.5),
	MaxClusterSize: config.RawInt("profile_builder.max_cluster_size", 100),

	ClickHouseAddr:     config.RawString("profile_builder.clickhouse_addr", "127.0.0.1:9000"),
	ClickHouseDatabase: config.RawString("profile_builder.clickhouse_database", "adtech"),
	ClickHouseUser:     config.RawString("profile_builder.clickhouse_user", "adtech"),
	ClickHousePassword: config.RawString("profile_builder.clickhouse_password", "adtech-local-dev"),
}
