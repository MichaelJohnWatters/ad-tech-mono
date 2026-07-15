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
	// reads behaviour/profile signals from, and writes identity_clusters
	// to, the same lake.
	DatalakeBucket config.StringKey
}{
	NATSURL:        config.RawString("batch_conductor.nats_url", routes.DefaultNATSURL),
	PipelineURL:    config.RawString("batch_conductor.pipeline_url", routes.DefaultPipelineURL),
	ReportingURL:   config.RawString("batch_conductor.reporting_url", routes.DefaultReportingURL),
	DatalakeBucket: config.RawString("batch_conductor.datalake_bucket", "adtech-datalake"),
}
