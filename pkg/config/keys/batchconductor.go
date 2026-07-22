package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// BatchConductor holds cmd/batch-conductor's keys. Raw: one-shot CronJob
// binary, registers no schema (nil Setup) — dayboundary posture.
var BatchConductor = struct {
	NATSURL      config.StringKey
	PipelineURL  config.StringKey
	ReportingURL config.StringKey
	// DatalakeBucket must match the pipeline's — the profile-builder step
	// writes identity_clusters to, and (fallback path only) reads behaviour/
	// profile signals from, the same lake.
	DatalakeBucket config.StringKey

	// ClickHouse* point the profile-builder step's phase-2 reads (behavioural
	// rule GROUP BY + reconcile) at the SAME ClickHouse the reporting service
	// writes behaviour_signals/profile_signals into (ADR 0006). Mirror the
	// reporting.clickhouse_* keys/defaults. Empty addr disables the querier →
	// fall back to the lake reads (the safety valve).
	ClickHouseAddr     config.StringKey
	ClickHouseDatabase config.StringKey
	ClickHouseUser     config.StringKey
	ClickHousePassword config.StringKey
}{
	NATSURL:        config.RawString("batch_conductor.nats_url", routes.DefaultNATSURL),
	PipelineURL:    config.RawString("batch_conductor.pipeline_url", routes.DefaultPipelineURL),
	ReportingURL:   config.RawString("batch_conductor.reporting_url", routes.DefaultReportingURL),
	DatalakeBucket: config.RawString("batch_conductor.datalake_bucket", "adtech-datalake"),

	ClickHouseAddr:     config.RawString("batch_conductor.clickhouse_addr", "127.0.0.1:9000"),
	ClickHouseDatabase: config.RawString("batch_conductor.clickhouse_database", "adtech"),
	ClickHouseUser:     config.RawString("batch_conductor.clickhouse_user", "adtech"),
	ClickHousePassword: config.RawString("batch_conductor.clickhouse_password", "adtech-local-dev"),
}
