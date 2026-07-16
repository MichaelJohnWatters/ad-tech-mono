package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var reportRunnerSet = config.NewKeySet(constants.ServiceReportRunner)

// ReportRunnerSchema is the report runner's owned schema — passed to
// config.Setup by cmd/report-runner. These keys previously squatted in the
// platform defaultSchema because the runner passed a nil schema to Setup.
func ReportRunnerSchema() []config.SchemaEntry { return reportRunnerSet.Entries() }

// ReportRunner holds the scheduled-report runner's config keys.
var ReportRunner = struct {
	NATSURL          config.StringKey
	ReportingURL     config.StringKey
	EmailFrom        config.StringKey
	SMTPHost         config.StringKey
	SMTPUsername     config.StringKey
	SMTPPassword     config.StringKey
	PollInterval     config.DurationKey
	ScheduleInterval config.DurationKey
	SweepInterval    config.DurationKey
	QueryTimeout     config.DurationKey
	ArtifactBucket   config.StringKey
	Retention        config.DurationKey
	StuckAfter       config.DurationKey
	PublicGatewayURL config.StringKey

	// Port is env/manifest territory by design — Raw, not in the schema.
	Port config.StringKey
}{
	NATSURL:          reportRunnerSet.String("report_runner.nats_url", routes.DefaultNATSURL, config.TierStatic, "NATS URL for report.completed announcements (webhooks delivery). Empty/unreachable = announcements skipped.", config.Since("v1.9")),
	ReportingURL:     reportRunnerSet.String("report_runner.reporting_url", "http://localhost:8086", config.TierStatic, "Reporting service base URL the report runner posts queries to.", config.Since("v1.3")),
	EmailFrom:        reportRunnerSet.String("report_runner.email_from", "reports@adtech.local", config.TierStatic, "From address on delivered scheduled-report emails.", config.Since("v1.3")),
	SMTPHost:         reportRunnerSet.String("report_runner.smtp_host", "", config.TierStatic, "SMTP host:port for scheduled-report delivery (Mailpit/SES). Empty → in-memory sender that only logs deliveries.", config.Since("v1.3")),
	SMTPUsername:     reportRunnerSet.String("report_runner.smtp_username", "", config.TierStatic, "SMTP username for authenticated delivery (SES/Sendgrid). Empty → unauthenticated (Mailpit).", config.Since("v1.4")),
	SMTPPassword:     reportRunnerSet.String("report_runner.smtp_password", "", config.TierSecret, "SMTP password for authenticated delivery (SES/Sendgrid). Secret; paired with report_runner.smtp_username.", config.Since("v1.4")),
	PollInterval:     reportRunnerSet.Duration("report_runner.poll_interval", "5s", config.TierLive, "How often the report-job executor polls the queue when idle (claimed jobs drain back-to-back).", config.Since("v1.5")),
	ScheduleInterval: reportRunnerSet.Duration("report_runner.schedule_interval", "60s", config.TierLive, "How often the scheduler tick enqueues due saved reports as jobs.", config.Since("v1.5")),
	SweepInterval:    reportRunnerSet.Duration("report_runner.sweep_interval", "1h", config.TierLive, "How often expired report artifacts + job rows are swept.", config.Since("v1.5")),
	QueryTimeout:     reportRunnerSet.Duration("report_runner.query_timeout", "10m", config.TierLive, "Per-job bound on the reporting query (async jobs may span the cold store; pair with reporting.query_timeout).", config.Since("v1.5")),
	ArtifactBucket:   reportRunnerSet.String("report_runner.artifact_bucket", "adtech-reports", config.TierStatic, "Object-store bucket for report artifacts. Private — downloads stream through the gateway after auth; never make this bucket public-read.", config.Since("v1.5")),
	Retention:        reportRunnerSet.Duration("report_runner.retention", "720h", config.TierLive, "How long completed report artifacts (and their job rows) are kept before the sweep removes them.", config.Since("v1.5")),
	StuckAfter:       reportRunnerSet.Duration("report_runner.stuck_after", "30m", config.TierLive, "Running jobs older than this are requeued on worker boot (crash recovery; safe with a single worker replica).", config.Since("v1.5")),
	PublicGatewayURL: reportRunnerSet.String("report_runner.public_gateway_url", "http://localhost:8080", config.TierStatic, "Public gateway base URL used to build download links in report-ready emails.", config.Since("v1.5")),

	Port: config.RawString("report_runner.port", routes.PortReportRunner),
}
