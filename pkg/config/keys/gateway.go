package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var gatewaySet = config.NewKeySet(constants.ServiceGateway)

// GatewaySchema is the Gateway schema — passed to config.Setup at boot.
func GatewaySchema() []config.SchemaEntry { return gatewaySet.Entries() }

// Gateway holds the Gateway config keys.
var Gateway = struct {
	JwtSigningKey      config.StringKey
	RequireAuth        config.BoolKey
	DSPURL             config.StringKey
	SSPURL             config.StringKey
	AdserverURL        config.StringKey
	ReportingURL       config.StringKey
	ExchangeURL        config.StringKey
	TrackerURL         config.StringKey
	PublicTrackerURL   config.StringKey
	JaegerURL          config.StringKey
	ConfigPollInterval config.DurationKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL  config.StringKey
	Port config.StringKey

	PublisherAdServerURL config.StringKey
	CreativesStoreURL    config.StringKey
	ServiceAPIKey        config.StringKey
	SSAIURL              config.StringKey
	PipelineURL          config.StringKey

	// Audience ingestion (ADR 0007): an upload is validated, staged, and
	// enqueued; small, due-now files run inline (200 + match rate), larger ones
	// are drained by the pipeline ingest worker (202 + job_id).
	IngestInlineMaxRows  config.IntKey
	IngestMaxUploadBytes config.IntKey

	// Ingest completion emails (ADR 0008 Feature 3): inline uploads notify from
	// the gateway when a job finishes. SMTP when smtp_host is set, else an
	// in-memory sender that only logs (dev).
	EmailFrom    config.StringKey
	SMTPHost     config.StringKey
	SMTPUsername config.StringKey
	SMTPPassword config.StringKey
}{
	JwtSigningKey:        gatewaySet.String("gateway.jwt_signing_key", "", config.TierSecret, "Fallback JWT signing key. Prefer an active jwt_signing secret in the secrets store (rotatable); this config key is the legacy/override path. Empty here AND no secret = auth bypassed (dev only — see gateway.require_auth).", config.Since("v1.0")),
	RequireAuth:          gatewaySet.Bool("gateway.require_auth", "false", config.TierStatic, "When true, the gateway refuses to boot unless a JWT signing key is available (from the secrets store or gateway.jwt_signing_key) — i.e. the dev auth-bypass is forbidden. Set true in staging/prod overlays so a missing key fails loud instead of silently granting admin to every request.", config.Since("v1.3")),
	DSPURL:               gatewaySet.String("gateway.dsp_url", "http://localhost:8082", config.TierStatic, "Internal DSP service URL the gateway proxies to for /v1/api/campaigns/*.", config.Since("v1.0")),
	SSPURL:               gatewaySet.String("gateway.ssp_url", "http://localhost:8084", config.TierStatic, "Internal SSP service URL the gateway proxies to for /v1/api/placements/*.", config.Since("v1.0")),
	AdserverURL:          gatewaySet.String("gateway.adserver_url", "http://localhost:8085", config.TierStatic, "Internal ad server URL the gateway proxies to for /v1/api/creatives/*.", config.Since("v1.0")),
	ReportingURL:         gatewaySet.String("gateway.reporting_url", "http://localhost:8086", config.TierStatic, "Internal reporting service URL the gateway proxies to for /v1/api/reports/* and /v1/api/billing/*.", config.Since("v1.0")),
	ExchangeURL:          gatewaySet.String("gateway.exchange_url", "http://localhost:8081", config.TierStatic, "Internal exchange URL the gateway proxies to for the OpenRTB try-it-out endpoints.", config.Since("v1.0")),
	TrackerURL:           gatewaySet.String("gateway.tracker_url", "http://localhost:8083", config.TierStatic, "Internal tracker URL the gateway proxies to for the developer pixel-trigger tools.", config.Since("v1.0")),
	PublicTrackerURL:     gatewaySet.String("gateway.public_tracker_url", "http://localhost:8083", config.TierStatic, "BROWSER-reachable tracker base baked into advertiser-embedded conversion pixels. Differs from tracker_url (which is the in-cluster proxy target); this one must resolve from the advertiser's own website. Dev: localhost:8083; prod: the public tracker domain.", config.Since("v1.11")),
	JaegerURL:            gatewaySet.String("gateway.jaeger_url", "http://localhost:16686", config.TierStatic, "Jaeger query API URL proxied for the browser. Needed because Jaeger v1.58 doesn't set CORS headers on its query endpoint.", config.Since("v1.0")),
	ConfigPollInterval:   gatewaySet.Duration("gateway.config_poll_interval", "30s", config.TierStatic, "How often the gateway polls Postgres for live-config changes. Same semantics as config.poll_interval but lets the gateway tune independently of the platform default.", config.Since("v1.0")),
	URL:                  config.RawString("gateway.url", routes.DefaultGatewayURL),
	Port:                 config.RawString("gateway.port", routes.PortGateway),
	PublisherAdServerURL: config.RawString("gateway.publisher_adserver_url", routes.DefaultPublisherAdServerURL),
	CreativesStoreURL:    config.RawString("gateway.creatives_store_url", ""),
	ServiceAPIKey:        config.RawString("gateway.service_api_key", "dev-api-key-do-not-use-in-prod"),
	SSAIURL:              config.RawString("gateway.ssai_url", routes.DefaultSSAIURL),
	// Pipeline hosts the Delta-lake profile summary the staff profile API reads.
	PipelineURL: config.RawString("gateway.pipeline_url", routes.DefaultPipelineURL),

	IngestInlineMaxRows:  gatewaySet.Int("gateway.ingest_inline_max_rows", "50000", config.TierLive, "Audience uploads at or below this row count (and due now) are matched INLINE in the request — instant match rate, 200. Larger files stage + enqueue and the pipeline ingest worker drains them (202 + job_id). Above this, only cheap validation stays synchronous; matching goes async (ADR 0007).", config.Since("v1.14")),
	IngestMaxUploadBytes: gatewaySet.Int("gateway.ingest_max_upload_bytes", "104857600", config.TierLive, "Hard cap on a single audience upload's raw size (bytes; default 100MB). Oversized uploads are rejected synchronously at intake before staging. Replaces the old 5MB multipart cap + row-count reject — large files now go through the same stage+enqueue path, not the drop-zone.", config.Since("v1.14")),

	EmailFrom:    gatewaySet.String("gateway.email_from", "audiences@adtech.local", config.TierStatic, "From address on audience-upload completion emails (ADR 0008). Inline uploads notify the uploader (+ additional_emails) when a job finishes.", config.Since("v1.15")),
	SMTPHost:     gatewaySet.String("gateway.smtp_host", "", config.TierStatic, "SMTP host:port for audience-upload completion emails (Mailpit/SES). Empty → in-memory sender that only logs deliveries.", config.Since("v1.15")),
	SMTPUsername: gatewaySet.String("gateway.smtp_username", "", config.TierStatic, "SMTP username for authenticated completion-email delivery (SES/Sendgrid). Empty → unauthenticated (Mailpit).", config.Since("v1.15")),
	SMTPPassword: gatewaySet.String("gateway.smtp_password", "", config.TierSecret, "SMTP password for authenticated completion-email delivery (SES/Sendgrid). Secret; paired with gateway.smtp_username.", config.Since("v1.15")),
}
