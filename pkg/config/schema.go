package config

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// Schema registry. Each service owns its own keys in cmd/<svc>/config.go
// and passes them to config.Setup at boot. Setup merges the service's slice
// + the platform-shared defaultSchema() into this in-process map and writes
// the per-pod row to service_registry.schema_entries (the on-the-wire form
// the config-manager UI reads). The map is the single source of truth for
// runtime validation; there is no separate Postgres "config_schema" table
// anymore.
//
// The platform-shared keys (server.*, nats.*, database.*, redis.*, s3.*,
// debug.*, otel.*, cache.warm.*) live in defaultSchema() — they're used by
// every service, so registering them per-service would just be repeated
// boilerplate.
var (
	schemaMu       sync.RWMutex
	schemaRegistry = map[string]SchemaEntry{}
)

func init() {
	for _, e := range defaultSchema() {
		schemaRegistry[e.Key] = e
	}
}

// Register merges a service's schema slice into the in-process registry.
// Called by Setup; tests with their own schema can call this directly.
// Service field is filled in from the serviceName arg when blank.
func Register(serviceName string, entries []SchemaEntry) {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	for _, e := range entries {
		if e.Service == "" {
			e.Service = serviceName
		}
		schemaRegistry[e.Key] = e
	}
}

// Tier classifies a config key by how/when it can be changed.
//
// Only Live keys are seeded into Postgres and rendered as editable in the
// UI. Static keys read from env/YAML/code-default only (changing them
// requires a redeploy). Secret keys read only from env (mounted from K8s
// Secret).
const (
	TierLive   = "live"   // operator-tunable at runtime, lives in Postgres `config` table
	TierStatic = "static" // infrastructure; env var or YAML overlay only
	TierSecret = "secret" // credentials; env var only (sourced from K8s Secret)
)

// SchemaEntry defines a config key's type, default, and validation.
// JSON tags so it round-trips through service_registry.schema_entries.
type SchemaEntry struct {
	Key         string `json:"key"`
	Type        string `json:"type"` // string, int, float, bool, duration
	Tier        string `json:"tier"` // live | static | secret
	Default     string `json:"default"`
	Description string `json:"description"`
	Service     string `json:"service"`
	Since       string `json:"since,omitempty"`
	Deprecated  bool   `json:"deprecated,omitempty"`
	ReplacedBy  string `json:"replaced_by,omitempty"`
}

// Schema returns the in-process schema registry as a slice. Each service's
// own schema (its cmd/<svc>/config.go file) + defaultSchema() are merged in
// via Register at boot. This is the source for local validation; the
// config-manager UI reads the union from service_registry.schema_entries
// instead.
func Schema() []SchemaEntry {
	schemaMu.RLock()
	defer schemaMu.RUnlock()
	out := make([]SchemaEntry, 0, len(schemaRegistry))
	for _, e := range schemaRegistry {
		out = append(out, e)
	}
	return out
}

// defaultSchema is the platform-shared key set: keys every service uses
// (server, nats, config, database, redis, s3, debug, otel, generic
// cache.warm.*). Kept here rather than registered per-service to avoid
// repeating the same rows in every cmd/<svc>/config.go.
//
// NOTE: *.port keys are NOT in the schema. Ports are infrastructure (K8s
// manifest / env var territory), not runtime-tunable config. Including
// them caused per-pod port overrides (DSP_PORT=8089) to lose to the
// seeded Postgres value. Defaults live in pkg/routes; env vars like
// GATEWAY_PORT / DSP_PORT override at boot.
func defaultSchema() []SchemaEntry {
	return []SchemaEntry{
		// Server timeouts — boot-time HTTP server config.
		{Key: "server.read_timeout", Type: "duration", Tier: TierStatic, Default: "5s", Description: "How long the HTTP server will wait for a client to send a request before timing it out.", Service: "platform", Since: "v1.0"},
		{Key: "server.write_timeout", Type: "duration", Tier: TierStatic, Default: "10s", Description: "How long the HTTP server will spend writing a response before timing out the client connection.", Service: "platform", Since: "v1.0"},
		{Key: "server.graceful_shutdown", Type: "duration", Tier: TierStatic, Default: "30s", Description: "How long shutdown waits for in-flight requests to drain before forcing the process to exit.", Service: "platform", Since: "v1.0"},

		// NATS reconnect / stream behavior — boot-time NATS client config.
		{Key: "nats.max_reconnects", Type: "int", Tier: TierStatic, Default: "30", Description: "Maximum times the NATS client retries connecting after losing the link before giving up.", Service: "platform", Since: "v1.0"},
		{Key: "nats.reconnect_wait", Type: "duration", Tier: TierStatic, Default: "1s", Description: "Backoff between NATS reconnect attempts.", Service: "platform", Since: "v1.0"},
		{Key: "nats.stream_max_age", Type: "duration", Tier: TierStatic, Default: "24h", Description: "How long JetStream keeps messages on a stream before discarding them.", Service: "platform", Since: "v1.0"},

		// Config manager — the poller itself can't reconfigure its own interval safely.
		{Key: "config.poll_interval", Type: "duration", Tier: TierStatic, Default: "30s", Description: "How often each pod polls Postgres for live-config changes. Lower = faster picks up of UI edits, higher = less DB traffic.", Service: "platform", Since: "v1.0"},

		// Database — connection settings are boot-time, credentials in URL are secret-adjacent.
		{Key: "database.url", Type: "string", Tier: TierStatic, Default: routes.DefaultPostgresURL, Description: "PostgreSQL connection URL used by every service. Set via DATABASE_URL env var in non-dev environments.", Service: "platform", Since: "v1.0"},
		{Key: "database.max_open_conns", Type: "int", Tier: TierLive, Default: "10", Description: "Maximum simultaneous Postgres connections this pod will open. Live: applied via db.SetMaxOpenConns on every change. Raise if you see connection-pool waits in traces.", Service: "platform", Since: "v1.0"},
		{Key: "database.max_idle_conns", Type: "int", Tier: TierLive, Default: "5", Description: "Maximum idle Postgres connections kept open between requests. Live: applied via db.SetMaxIdleConns on every change. Lower trims footprint, higher cuts reconnect cost.", Service: "platform", Since: "v1.0"},

		// Redis — connection is infra; password is secret.
		{Key: "redis.url", Type: "string", Tier: TierStatic, Default: routes.DefaultRedisAddr, Description: "Redis address (host:port). Used for L2 cache, budgets, frequency caps, and dedup keys.", Service: "platform", Since: "v1.0"},
		{Key: "redis.password", Type: "string", Tier: TierSecret, Default: "", Description: "Redis password. Empty in dev (no auth); sourced from K8s Secret in staging/prod.", Service: "platform", Since: "v1.1"},
		{Key: "redis.db", Type: "int", Tier: TierStatic, Default: "0", Description: "Redis logical DB number (0-15). Use a non-zero DB to isolate environments sharing one Redis.", Service: "platform", Since: "v1.1"},
		{Key: "redis.pool_size", Type: "int", Tier: TierStatic, Default: "10", Description: "Maximum simultaneous Redis connections this pod will open. Raise for high-RPS pods seeing pool waits.", Service: "platform", Since: "v1.0"},

		// Minio/S3 — endpoint + bucket = infra, secret_key = secret.
		{Key: "s3.endpoint", Type: "string", Tier: TierStatic, Default: routes.DefaultMinioEndpoint, Description: "S3/Minio endpoint. Locally this is Minio on 9000; in prod set to s3.amazonaws.com or the region-specific endpoint.", Service: "platform", Since: "v1.0"},
		{Key: "s3.access_key", Type: "string", Tier: TierStatic, Default: "adtech", Description: "S3/Minio access key (IAM user / Minio root user).", Service: "platform", Since: "v1.0"},
		{Key: "s3.secret_key", Type: "string", Tier: TierSecret, Default: "adtech-local-dev", Description: "S3/Minio secret key. Sourced from K8s Secret in non-dev environments.", Service: "platform", Since: "v1.0"},
		{Key: "s3.bucket", Type: "string", Tier: TierStatic, Default: "adtech-creatives", Description: "Bucket name that holds creative HTML/image assets.", Service: "platform", Since: "v1.0"},
		{Key: "s3.region", Type: "string", Tier: TierStatic, Default: "us-east-1", Description: "AWS region for S3. Minio ignores this; required for real S3.", Service: "platform", Since: "v1.1"},
		{Key: "s3.use_ssl", Type: "bool", Tier: TierStatic, Default: "false", Description: "Connect to S3/Minio over HTTPS. False for local Minio, true for real S3.", Service: "platform", Since: "v1.0"},

		// Debug endpoints (static — registering/unregistering routes requires restart).
		{Key: "debug.endpoints_enabled", Type: "bool", Tier: TierStatic, Default: "true", Description: "Expose /debug/* routes (warm-cache refresh, internal state dumps). Must be false in production.", Service: "platform", Since: "v1.1"},

		// OpenTelemetry / tracing (static — exporter/sampler are built at Init).
		{Key: "otel.endpoint", Type: "string", Tier: TierStatic, Default: "localhost:4318", Description: "OTLP/HTTP collector endpoint (Jaeger in dev). Leave empty to disable tracing entirely.", Service: "platform", Since: "v1.1"},
		{Key: "otel.sample_ratio", Type: "float", Tier: TierStatic, Default: "1.0", Description: "Fraction of traces to record (0.0-1.0). 1.0 samples every request; lower in high-RPS services like the tracker.", Service: "platform", Since: "v1.1"},
		{Key: "otel.service_version", Type: "string", Tier: TierStatic, Default: "dev", Description: "Version string reported to the OTel collector. Usually set to the build SHA at deploy time.", Service: "platform", Since: "v1.1"},

		// Warm cache default poll interval — fallback used by services without
		// per-cache overrides. Per-cache overrides (cache.warm.campaigns.*,
		// cache.warm.creatives.*, etc.) live in the owning service's
		// cmd/<svc>/config.go and override this default at boot.
		{Key: "cache.warm.poll_interval", Type: "duration", Tier: TierStatic, Default: "30s", Description: "Fallback refresh interval for warm caches with no per-cache override. Most caches set their own.", Service: "platform", Since: "v1.1"},
		{Key: "cache.warm.freq_caps.poll_interval", Type: "duration", Tier: TierLive, Default: "30s", Description: "Ad-server per-campaign frequency-cap warm-cache refresh. Campaign PATCH invalidates via the campaigns subject; this bounds staleness otherwise.", Service: "adserver", Since: "v1.3"},

		// Scheduled-report runner (cmd/report-runner).
		{Key: "report_runner.reporting_url", Type: "string", Tier: TierStatic, Default: "http://localhost:8086", Description: "Reporting service base URL the report runner posts queries to.", Service: "report-runner", Since: "v1.3"},
		{Key: "report_runner.email_from", Type: "string", Tier: TierStatic, Default: "reports@adtech.local", Description: "From address on delivered scheduled-report emails.", Service: "report-runner", Since: "v1.3"},
		{Key: "report_runner.smtp_host", Type: "string", Tier: TierStatic, Default: "", Description: "SMTP host:port for scheduled-report delivery (Mailpit/SES). Empty → in-memory sender that only logs deliveries.", Service: "report-runner", Since: "v1.3"},

		// Money loop (prepay balance gating + drawdown).
		{Key: "cache.warm.advertiser_balances.poll_interval", Type: "duration", Tier: TierLive, Default: "30s", Description: "DSP balance warm-cache refresh. NATS invalidates (topup/drawdown) make this the fallback bound on balance staleness.", Service: "dsp", Since: "v1.2"},
		{Key: "dsp.balance_gate_enabled", Type: "bool", Tier: TierLive, Default: "true", Description: "Gate bidding on the advertiser prepay balance (no funds -> no bid). Rollout escape hatch; disabling reverts to daily-budget-only enforcement.", Service: "dsp", Since: "v1.2"},
		{Key: "billing.balance_invalidate_min_interval", Type: "duration", Tier: TierLive, Default: "5s", Description: "Per-account throttle on the advertiser-balance cache invalidates published by the billing drawdown sink.", Service: "reporting", Since: "v1.2"},
	}
}

// ErrValidation is the sentinel returned (via errors.Is) when a value
// fails schema-type validation. The gateway PUT handler uses this to
// distinguish client-input errors from server-side persistence failures
// and return 400 instead of 500.
var ErrValidation = errors.New("schema validation failed")

// Validate checks if a value is valid for a given config key.
func Validate(key, value string) error {
	for _, entry := range Schema() {
		if entry.Key == key {
			return validateType(entry, value)
		}
	}
	// Unknown keys are allowed (forward compatibility)
	return nil
}

func validateType(entry SchemaEntry, value string) error {
	switch entry.Type {
	case "duration":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("%w: key %q expects a duration (e.g. 100ms, 5s), got %q", ErrValidation, entry.Key, value)
		}
	case "int":
		for _, c := range value {
			if c < '0' || c > '9' {
				return fmt.Errorf("%w: key %q expects an integer, got %q", ErrValidation, entry.Key, value)
			}
		}
	case "float":
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("%w: key %q expects a number, got %q", ErrValidation, entry.Key, value)
		}
	case "bool":
		if value != "true" && value != "false" {
			return fmt.Errorf("%w: key %q expects true or false, got %q", ErrValidation, entry.Key, value)
		}
	}
	return nil
}
