package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var pipelineSet = config.NewKeySet(constants.ServicePipeline)

// PipelineSchema is the Pipeline schema — passed to config.Setup at boot.
func PipelineSchema() []config.SchemaEntry { return pipelineSet.Entries() }

// Pipeline holds the data-pipeline service's config keys. Registered (was
// raw) so they appear in the config-manager UI — the onboarding retention in
// particular is an operator dial, not an env-var-only constant.
var Pipeline = struct {
	Port                  config.StringKey
	NATSURL               config.StringKey
	DatalakeBucket        config.StringKey
	DatalakeBatchSize     config.IntKey
	DatalakeFlushInterval config.DurationKey
	OnboardingEnabled     config.BoolKey
	OnboardingBucket      config.StringKey
	OnboardingPollEvery   config.DurationKey
	OnboardingRetention   config.DurationKey
	IngestWorkerInterval  config.DurationKey
}{
	Port:                  pipelineSet.String("pipeline.port", routes.PortPipeline, config.TierStatic, "HTTP port for /healthz, /readyz, the datalake debug snapshot, and the lake maintenance endpoints (purge/compact/vacuum/profile).", config.Since("v1.9")),
	NATSURL:               pipelineSet.String("pipeline.nats_url", routes.DefaultNATSURL, config.TierStatic, "NATS JetStream URL the datalake sink consumes the event stream from.", config.Since("v1.9")),
	DatalakeBucket:        pipelineSet.String("pipeline.datalake_bucket", "adtech-datalake", config.TierStatic, "Object-store bucket holding the Delta lake tables. Must match every lake reader (reporting cold store, profile-builder, batch-conductor).", config.Since("v1.9")),
	DatalakeBatchSize:     pipelineSet.Int("pipeline.datalake_batch_size", "500", config.TierStatic, "Buffered events per table before an early Parquet flush (the flush interval flushes smaller batches).", config.Since("v1.9")),
	DatalakeFlushInterval: pipelineSet.Duration("pipeline.datalake_flush_interval", "15s", config.TierStatic, "Periodic flush of buffered events to Parquet. Keep comfortably UNDER the 30s NATS AckWait: with ack-after-flush, a flush interval >= AckWait redelivers before we ack.", config.Since("v1.9")),
	OnboardingEnabled:     pipelineSet.Bool("pipeline.onboarding_enabled", "true", config.TierStatic, "Run the third-party audience drop-zone poller ({provider}/incoming/ in the onboarding bucket → validate → memberships + profile_signals; rejects quarantined to {provider}/rejected/).", config.Since("v1.9")),
	OnboardingBucket:      pipelineSet.String("pipeline.onboarding_bucket", "adtech-onboarding", config.TierStatic, "Drop-zone bucket providers deliver audience files into (csv/tsv/parquet, zip+gzip auto-detected).", config.Since("v1.9")),
	OnboardingPollEvery:   pipelineSet.Duration("pipeline.onboarding_poll_interval", "30s", config.TierStatic, "How often the drop-zone poller Lists the onboarding bucket for new files (Minio S3-event support isn't assumed).", config.Since("v1.9")),
	OnboardingRetention:   pipelineSet.Duration("pipeline.onboarding_retention", "720h", config.TierLive, "How long processed/rejected drop-zone artifact BYTES are retained before the sweep deletes them (the onboarding_runs row survives, stamped swept_at). Live: re-read every poll tick, so a change applies without a restart. The lake's profile_signals is the replayable record — these copies are audit/debug only.", config.Since("v1.9")),
	IngestWorkerInterval:  pipelineSet.Duration("pipeline.ingest_worker_interval", "5s", config.TierStatic, "How often the audience ingest worker reclaims lapsed leases and drains the audience_ingest_jobs queue (ADR 0007). Distinct from the drop-zone poll interval: the poller enqueues jobs, this worker processes them.", config.Since("v1.13")),
}
